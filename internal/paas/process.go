package paas

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	stopTimeout   = 10 * time.Second
	maxBackoff    = 30 * time.Second
	stableRuntime = 30 * time.Second
	logLines      = 500
)

// Process states.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateCrashed  = "crashed"
	StateStopped  = "stopped"
)

// process supervises one app binary: it runs it, restarts it with backoff
// when it exits, and captures its output.
type process struct {
	name    string
	bin     string
	dir     string
	port    int
	env     []string
	logs    *ring
	stopCh  chan struct{}
	done    chan struct{}
	stopper sync.Once

	mu       sync.Mutex
	cmd      *exec.Cmd
	state    string
	exits    int
	lastExit string
}

func newProcess(name, bin, dir string, port int, env []string, logs *ring) *process {
	return &process{
		name:   name,
		bin:    bin,
		dir:    dir,
		port:   port,
		env:    env,
		logs:   logs,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
		state:  StateStarting,
	}
}

func (p *process) start() { go p.loop() }

func (p *process) loop() {
	defer close(p.done)
	backoff := time.Second
	for {
		started := time.Now()
		err := p.runOnce()
		select {
		case <-p.stopCh:
			p.setState(StateStopped)
			return
		default:
		}
		if time.Since(started) > stableRuntime {
			backoff = time.Second
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		p.logs.add(fmt.Sprintf("[tinypaas] process %s; restarting in %s", msg, backoff))
		p.mu.Lock()
		p.state = StateCrashed
		p.lastExit = msg
		p.mu.Unlock()
		select {
		case <-p.stopCh:
			p.setState(StateStopped)
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (p *process) runOnce() error {
	w := &lineWriter{fn: func(line string) {
		p.logs.add(line)
		log.Printf("[%s] %s", p.name, line)
	}}
	defer w.flush()

	cmd := exec.Command(p.bin)
	cmd.Dir = p.dir
	cmd.Env = p.env
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.SysProcAttr = sysProcAttr()

	p.mu.Lock()
	select {
	case <-p.stopCh:
		p.mu.Unlock()
		return nil
	default:
	}
	if err := cmd.Start(); err != nil {
		p.exits++
		p.mu.Unlock()
		return err
	}
	p.cmd = cmd
	p.state = StateStarting
	p.mu.Unlock()

	go p.markRunningWhenListening(cmd)

	err := cmd.Wait()
	p.mu.Lock()
	p.cmd = nil
	p.exits++
	p.mu.Unlock()
	return err
}

// markRunningWhenListening flips the state to running once the port accepts
// connections, for as long as cmd is still the current process.
func (p *process) markRunningWhenListening(cmd *exec.Cmd) {
	for {
		p.mu.Lock()
		current := p.cmd == cmd
		p.mu.Unlock()
		if !current {
			return
		}
		if p.listening() {
			p.mu.Lock()
			if p.cmd == cmd {
				p.state = StateRunning
			}
			p.mu.Unlock()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (p *process) listening() bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// waitHealthy waits until the app accepts connections, failing early if the
// process exits.
func (p *process) waitHealthy(timeout time.Duration) error {
	p.mu.Lock()
	exits := p.exits
	p.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		exited, last, running := p.exits > exits, p.lastExit, p.state == StateRunning
		p.mu.Unlock()
		if exited {
			if last == "" {
				last = "exited"
			}
			return fmt.Errorf("app %s before listening on $PORT", last)
		}
		if running {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("app did not listen on 127.0.0.1:%d within %s", p.port, timeout)
}

func (p *process) stop() {
	p.stopper.Do(func() { close(p.stopCh) })
	p.signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopTimeout):
		p.signal(syscall.SIGKILL)
		<-p.done
	}
}

func (p *process) signal(sig syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		// negative pid: signal the whole process group
		_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	}
}

func (p *process) setState(s string) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
}

func (p *process) status() (state string, restarts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, p.exits
}

// ring is a bounded in-memory log buffer.
type ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func newRing(n int) *ring { return &ring{lines: make([]string, n)} }

func (r *ring) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines[r.next] = time.Now().Format(time.RFC3339) + " " + line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
	}
	all = append(all, r.lines[:r.next]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// lineWriter splits written bytes into lines.
type lineWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	fn  func(string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(b)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(w.buf.Next(i+1), "\r\n"))
		w.fn(line)
	}
	if w.buf.Len() > 64*1024 {
		w.fn(w.buf.String())
		w.buf.Reset()
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.fn(w.buf.String())
		w.buf.Reset()
	}
}

func baseEnv(name, dataDir string, port int) []string {
	env := []string{
		"PORT=" + strconv.Itoa(port),
		"HOME=" + dataDir,
		"TINYPAAS_APP=" + name,
		"TINYPAAS_BASE_PATH=/" + name,
		"TINYPAAS_DATA_DIR=" + dataDir,
	}
	for _, k := range []string{"PATH", "TZ", "LANG", "LC_ALL", "SSL_CERT_FILE", "TZDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
