// Command tinypaas runs the tinypaas daemon and manages its apps.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"tinypaas/internal/paas"
	"tinypaas/internal/scaffold"
)

const usage = `tinypaas - run tiny web apps behind one OIDC-protected domain

Usage:
  tinypaas new [-title T] [-description D] [-dir DIR] NAME
                                       scaffold a new app (Go + SQLite + Nix + CI)
  tinypaas serve [flags]               run the daemon
  tinypaas deploy [-manifest app.json] BINARY
                                       deploy (or update) an app
  tinypaas ls                          list apps
  tinypaas logs [-n 100] NAME          show recent app output
  tinypaas restart NAME                restart an app
  tinypaas rollback NAME               swap to the previous release
  tinypaas rm [-purge] NAME            remove an app (-purge also deletes its data)

Client commands talk to the daemon over $TINYPAAS_SOCKET
(default /run/tinypaas/control.sock).
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "new":
		err = newApp(args)
	case "serve":
		err = serve(args)
	case "deploy":
		err = deploy(args)
	case "ls", "list":
		err = list()
	case "logs":
		err = logs(args)
	case "restart", "rollback":
		err = simple(cmd, args, http.MethodPost, "/apps/%s/"+cmd)
	case "rm", "remove":
		err = remove(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var cfg paas.Config
	var clientIDFile, clientSecretFile, groups string
	fs.StringVar(&cfg.StateDir, "state-dir", "/var/lib/tinypaas", "state directory")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8090", "HTTP listen address")
	fs.StringVar(&cfg.Socket, "socket", socketPath(), "control socket path")
	fs.StringVar(&cfg.PublicURL, "public-url", "", "public base URL, e.g. https://apps.example.com")
	fs.IntVar(&cfg.PortBase, "port-base", 20000, "first port assigned to apps")
	fs.StringVar(&cfg.OIDC.Issuer, "oidc-issuer", "", "OIDC issuer URL")
	fs.StringVar(&clientIDFile, "oidc-client-id-file", "", "file containing the OIDC client id")
	fs.StringVar(&clientSecretFile, "oidc-client-secret-file", "", "file containing the OIDC client secret")
	fs.StringVar(&groups, "allowed-groups", "", "comma-separated groups allowed to sign in (empty: any user)")
	fs.BoolVar(&cfg.NoAuth, "no-auth", false, "DEVELOPMENT ONLY: disable login")
	_ = fs.Parse(args)

	var err error
	if cfg.OIDC.ClientID, err = readSecret(clientIDFile); err != nil {
		return err
	}
	if cfg.OIDC.ClientSecret, err = readSecret(clientSecretFile); err != nil {
		return err
	}
	for _, g := range strings.Split(groups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			cfg.OIDC.AllowedGroups = append(cfg.OIDC.AllowedGroups, g)
		}
	}
	srv, err := paas.NewServer(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

func newApp(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	title := fs.String("title", "", "display title (default: derived from NAME)")
	desc := fs.String("description", "", "one-line description for the apps index")
	dir := fs.String("dir", "", "target directory (default: ./NAME)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: tinypaas new [-title T] [-description D] [-dir DIR] NAME")
	}
	name := fs.Arg(0)
	if !paas.ValidName(name) {
		return fmt.Errorf("invalid app name %q: use a lowercase slug like meal-picker", name)
	}
	if *title == "" {
		*title = scaffold.TitleFromName(name)
	}
	if *dir == "" {
		*dir = name
	}
	if err := scaffold.Write(*dir, scaffold.Data{Name: name, Title: *title, Description: *desc}); err != nil {
		return err
	}
	fmt.Printf("created %s in %s (see AGENTS.md)\n", name, *dir)
	return nil
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

func deploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	manifestPath := fs.String("manifest", "app.json", "path to app.json")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: tinypaas deploy [-manifest app.json] BINARY")
	}
	man, err := paas.ReadManifest(*manifestPath)
	if err != nil {
		return err
	}
	bin, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer bin.Close()
	if fi, err := bin.Stat(); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", fs.Arg(0))
	}

	// Stream the multipart body: manifest first, then the binary.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			mf, err := mw.CreateFormField("manifest")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(mf).Encode(man); err != nil {
				return err
			}
			bf, err := mw.CreateFormFile("binary", man.Name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(bf, bin); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	fmt.Printf("deploying %s...\n", man.Name)
	req, _ := http.NewRequest(http.MethodPost, "http://tinypaas/deploy", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(req, os.Stdout)
}

func list() error {
	req, _ := http.NewRequest(http.MethodGet, "http://tinypaas/apps", nil)
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	var apps []paas.AppStatus
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tPORT\tRESTARTS\tDEPLOYED\tTITLE")
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", a.Name, a.State, a.Port, a.Restarts,
			a.DeployedAt.Format("2006-01-02 15:04"), a.Title)
	}
	return tw.Flush()
}

func logs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 100, "number of lines")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: tinypaas logs [-n 100] NAME")
	}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://tinypaas/apps/%s/logs?n=%d", fs.Arg(0), *n), nil)
	return do(req, os.Stdout)
}

func remove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the app's data directory")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: tinypaas rm [-purge] NAME")
	}
	url := "http://tinypaas/apps/" + fs.Arg(0)
	if *purge {
		url += "?purge=1"
	}
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	return do(req, os.Stdout)
}

func simple(cmd string, args []string, method, pathFmt string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: tinypaas %s NAME", cmd)
	}
	req, _ := http.NewRequest(method, "http://tinypaas"+fmt.Sprintf(pathFmt, args[0]), nil)
	return do(req, os.Stdout)
}

func do(req *http.Request, out io.Writer) error {
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func socketPath() string {
	if s := os.Getenv("TINYPAAS_SOCKET"); s != "" {
		return s
	}
	return "/run/tinypaas/control.sock"
}

func client() *http.Client {
	sock := socketPath()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}
