package paas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State directory layout:
//
//	<state>/apps/<name>/app.json       manifest of the current release
//	<state>/apps/<name>/current        current binary
//	<state>/apps/<name>/previous(.json) previous release, for rollback
//	<state>/apps/<name>/port           assigned port
//	<state>/data/<name>/               app working directory (persistent)

// ErrNotFound is returned for unknown apps.
var ErrNotFound = errors.New("app not found")

const healthTimeout = 30 * time.Second

type app struct {
	manifest   Manifest
	port       int
	deployedAt time.Time
	proc       *process
	logs       *ring
	proxy      *httputil.ReverseProxy
}

// AppStatus is the public view of an app.
type AppStatus struct {
	Manifest
	Port        int       `json:"port"`
	State       string    `json:"state"`
	Restarts    int       `json:"restarts"`
	HasPrevious bool      `json:"hasPrevious"`
	DeployedAt  time.Time `json:"deployedAt"`
}

// Manager owns the set of deployed apps and their processes.
type Manager struct {
	root     string
	portBase int
	newProxy func(name string, port int) *httputil.ReverseProxy

	opMu sync.Mutex // serialises deploy/rollback/remove/restart
	mu   sync.RWMutex
	apps map[string]*app
}

func newManager(root string, portBase int, newProxy func(string, int) *httputil.ReverseProxy) (*Manager, error) {
	for _, d := range []string{"apps", "data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return nil, err
		}
	}
	return &Manager{root: root, portBase: portBase, newProxy: newProxy, apps: map[string]*app{}}, nil
}

func (m *Manager) appDir(name string) string  { return filepath.Join(m.root, "apps", name) }
func (m *Manager) dataDir(name string) string { return filepath.Join(m.root, "data", name) }

// LoadAll starts every app found in the state directory.
func (m *Manager) LoadAll() {
	entries, err := os.ReadDir(filepath.Join(m.root, "apps"))
	if err != nil {
		log.Printf("load apps: %v", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !ValidName(name) {
			continue
		}
		if err := m.load(name); err != nil {
			log.Printf("load app %s: %v", name, err)
		}
	}
}

func (m *Manager) load(name string) error {
	dir := m.appDir(name)
	man, err := ReadManifest(filepath.Join(dir, "app.json"))
	if err != nil {
		return err
	}
	port, err := m.readPort(name)
	if err != nil {
		return err
	}
	fi, err := os.Stat(filepath.Join(dir, "current"))
	if err != nil {
		return err
	}
	a := &app{manifest: man, port: port, deployedAt: fi.ModTime(), logs: newRing(logLines)}
	a.proxy = m.newProxy(name, port)
	m.startProc(a)
	m.mu.Lock()
	m.apps[name] = a
	m.mu.Unlock()
	return nil
}

func (m *Manager) startProc(a *app) {
	name := a.manifest.Name
	data := m.dataDir(name)
	p := newProcess(name, filepath.Join(m.appDir(name), "current"), data, a.port, baseEnv(name, data, a.port), a.logs)
	m.mu.Lock()
	a.proc = p
	m.mu.Unlock()
	p.start()
}

func (m *Manager) readPort(name string) (int, error) {
	b, err := os.ReadFile(filepath.Join(m.appDir(name), "port"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func (m *Manager) allocatePort() int {
	used := map[int]bool{}
	entries, _ := os.ReadDir(filepath.Join(m.root, "apps"))
	for _, e := range entries {
		if p, err := m.readPort(e.Name()); err == nil {
			used[p] = true
		}
	}
	p := m.portBase
	for used[p] {
		p++
	}
	return p
}

// Get returns the app with the given name, if deployed.
func (m *Manager) get(name string) (*app, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.apps[name]
	return a, ok
}

// List returns all apps sorted by title.
func (m *Manager) List() []AppStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AppStatus, 0, len(m.apps))
	for name, a := range m.apps {
		state, restarts := a.proc.status()
		_, err := os.Stat(filepath.Join(m.appDir(name), "previous"))
		out = append(out, AppStatus{
			Manifest:    a.manifest,
			Port:        a.port,
			State:       state,
			Restarts:    restarts,
			HasPrevious: err == nil,
			DeployedAt:  a.deployedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out
}

// Logs returns the last n log lines of an app.
func (m *Manager) Logs(name string, n int) ([]string, error) {
	a, ok := m.get(name)
	if !ok {
		return nil, ErrNotFound
	}
	return a.logs.tail(n), nil
}

// Deploy installs a new release of an app and (re)starts it. If the new
// release fails to come up, the previous release is restored.
func (m *Manager) Deploy(man Manifest, bin io.Reader) error {
	if err := man.Validate(); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	name := man.Name
	dir := m.appDir(name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dataDir(name), 0o750); err != nil {
		return err
	}

	// Write the incoming binary first so a failed upload changes nothing.
	incoming := filepath.Join(dir, "incoming")
	if err := writeFile(incoming, bin, 0o755); err != nil {
		return fmt.Errorf("store binary: %w", err)
	}
	manJSON, _ := json.MarshalIndent(man, "", "  ")

	port, err := m.readPort(name)
	if err != nil {
		port = m.allocatePort()
		if err := os.WriteFile(filepath.Join(dir, "port"), []byte(strconv.Itoa(port)+"\n"), 0o640); err != nil {
			return err
		}
	}

	old, existed := m.get(name)
	if existed {
		old.proc.stop()
	}

	// Keep the existing previous release stashed until the new one is healthy,
	// so a failed deploy leaves both rollback targets untouched.
	hadCurrent := exists(filepath.Join(dir, "current"))
	hadPrevious := exists(filepath.Join(dir, "previous"))
	if hadPrevious {
		if err := rotate(dir, "previous", "stash"); err != nil {
			return err
		}
	}
	if hadCurrent {
		if err := rotate(dir, "current", "previous"); err != nil {
			return err
		}
	}
	if err := os.Rename(incoming, filepath.Join(dir, "current")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), append(manJSON, '\n'), 0o640); err != nil {
		return err
	}

	a := &app{manifest: man, port: port, deployedAt: time.Now(), logs: newRing(logLines)}
	if existed {
		a.logs = old.logs
	}
	a.proxy = m.newProxy(name, port)
	a.logs.add("[tinypaas] deploying new release")
	m.startProc(a)
	m.mu.Lock()
	m.apps[name] = a
	m.mu.Unlock()

	if err := a.proc.waitHealthy(healthTimeout); err != nil {
		if !hadCurrent {
			return fmt.Errorf("%w (app left crash-looping; see logs)", err)
		}
		a.logs.add("[tinypaas] new release unhealthy, restoring previous release: " + err.Error())
		a.proc.stop()
		if rerr := rotate(dir, "previous", "current"); rerr != nil {
			return fmt.Errorf("%w; rollback also failed: %v", err, rerr)
		}
		if hadPrevious {
			_ = rotate(dir, "stash", "previous")
		}
		if prev, perr := ReadManifest(filepath.Join(dir, "app.json")); perr == nil {
			m.mu.Lock()
			a.manifest = prev
			m.mu.Unlock()
		}
		m.startProc(a)
		if herr := a.proc.waitHealthy(healthTimeout); herr != nil {
			return fmt.Errorf("%w; previous release restored but unhealthy: %v", err, herr)
		}
		return fmt.Errorf("%w (previous release restored)", err)
	}
	os.Remove(filepath.Join(dir, "stash"))
	os.Remove(filepath.Join(dir, "stash.json"))
	return nil
}

// Rollback swaps the current and previous releases.
func (m *Manager) Rollback(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	a, ok := m.get(name)
	if !ok {
		return ErrNotFound
	}
	dir := m.appDir(name)
	if !exists(filepath.Join(dir, "previous")) {
		return errors.New("no previous release")
	}
	a.proc.stop()
	if err := swapReleases(dir); err != nil {
		m.startProc(a)
		return err
	}
	m.mu.Lock()
	if man, err := ReadManifest(filepath.Join(dir, "app.json")); err == nil {
		a.manifest = man
	}
	a.deployedAt = time.Now()
	m.mu.Unlock()
	a.logs.add("[tinypaas] rolled back")
	m.startProc(a)
	return a.proc.waitHealthy(healthTimeout)
}

// Restart restarts an app's process.
func (m *Manager) Restart(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	a, ok := m.get(name)
	if !ok {
		return ErrNotFound
	}
	a.proc.stop()
	a.logs.add("[tinypaas] restarting")
	m.startProc(a)
	return a.proc.waitHealthy(healthTimeout)
}

// Remove stops and deletes an app. Data is kept unless purge is set.
func (m *Manager) Remove(name string, purge bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	a, ok := m.get(name)
	if !ok {
		return ErrNotFound
	}
	a.proc.stop()
	m.mu.Lock()
	delete(m.apps, name)
	m.mu.Unlock()
	if err := os.RemoveAll(m.appDir(name)); err != nil {
		return err
	}
	if purge {
		return os.RemoveAll(m.dataDir(name))
	}
	return nil
}

// StopAll stops every app (used on shutdown).
func (m *Manager) StopAll() {
	m.mu.RLock()
	var wg sync.WaitGroup
	for _, a := range m.apps {
		wg.Add(1)
		go func(p *process) { defer wg.Done(); p.stop() }(a.proc)
	}
	m.mu.RUnlock()
	wg.Wait()
}

// rotate moves the release `from` (binary + manifest) to `to`.
func rotate(dir, from, to string) error {
	if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, manifestFor(from)), filepath.Join(dir, manifestFor(to)))
}

func manifestFor(release string) string {
	if release == "current" {
		return "app.json"
	}
	return release + ".json"
}

func swapReleases(dir string) error {
	if err := rotate(dir, "current", "swap"); err != nil {
		return err
	}
	if err := rotate(dir, "previous", "current"); err != nil {
		return err
	}
	return rotate(dir, "swap", "previous")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// upstream returns the URL of an app's local listener.
func upstream(port int) *url.URL {
	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
}
