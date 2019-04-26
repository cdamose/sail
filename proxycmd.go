package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"go.coder.com/flog"
	"golang.org/x/xerrors"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func codeServerProxy(w http.ResponseWriter, r *http.Request, port string) {
	rp := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   "localhost:" + port,
	})
	rp.ModifyResponse = func(resp *http.Response) error {
		if r.URL.Path != "/" || resp.Header.Get("Upgrade") == "websocket" {
			return nil
		}
		defer resp.Body.Close()

		if resp.Header.Get("Content-Encoding") == "gzip" {
			r, err := gzip.NewReader(resp.Body)
			if err != nil {
				return xerrors.Errorf("failed to create gzip reader: %w", err)
			}
			resp.Body = ioutil.NopCloser(r)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return xerrors.Errorf("failed to read body: %w", err)
		}

		b = bytes.Replace(b, []byte(`<head>`), []byte(`<head>
<script src="sail.js"></script>
`), 1)
		if resp.Header.Get("Content-Encoding") == "gzip" {
			var bb bytes.Buffer
			w := gzip.NewWriter(&bb)
			_, err = w.Write(b)
			if err != nil {
				return xerrors.Errorf("failed to gzip b: %w", err)
			}
			err = w.Flush()
			if err != nil {
				return xerrors.Errorf("failed to flush gzip writer: %w", err)
			}
			resp.Header.Set("Content-Length", strconv.Itoa(bb.Len()))
			b = bb.Bytes()
		}
		resp.Body = ioutil.NopCloser(bytes.NewReader(b))
		return nil
	}
	rp.ServeHTTP(w, r)
}

type proxy struct {
	url        string
	cntName    string
	refreshing int64

	mu             sync.Mutex
	codeServerPort string
	portErr        error
}

func (p *proxy) getCodeServerPort() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.codeServerPort, p.portErr
}

func (p *proxy) refreshPort() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	atomic.StoreInt64(&p.refreshing, 1)
	defer atomic.StoreInt64(&p.refreshing, 0)

	for {
		port, err := codeServerPort(p.cntName)
		p.mu.Lock()
		p.codeServerPort = port
		p.portErr = err
		p.mu.Unlock()
		if err == nil {
			return
		}

		time.Sleep(time.Millisecond * 100)
		if ctx.Err() != nil {
			flog.Fatal("failed to refresh code-server port: %v", err)
		}
	}
}

func (p *proxy) shouldDie() error {
	cli := dockerClient()
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	cnt, err := cli.ContainerInspect(ctx, p.cntName)
	if err != nil {
		return xerrors.Errorf("failed to inspect container: %w", err)
	}

	if cnt.Config.Labels[proxyURLLabel] != p.url {
		return xerrors.Errorf("container is being serviced by a different proxy")
	}

	if cnt.State.Status != "running" {
		return xerrors.Errorf("container is not running: %v", cnt.State.Status)
	}

	return nil
}

func (p *proxy) gc() {
	t := time.NewTicker(time.Second * 30)
	defer t.Stop()

	errs := 0
	for range t.C {
		err := p.shouldDie()
		if err != nil {
			flog.Error("%v", err)
			errs++
		} else {
			errs = 0
		}
		// On the 5th error we fatal. We wait till the 5th in case
		// the container is being restarted.
		if errs == 5 {
			flog.Fatal("terminating due to too many should die errors")
		}
	}
}

func (p *proxy) reload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()

	sail := exec.CommandContext(ctx, os.Args[0], "edit", toSailName(p.cntName))
	sail.Env = append(os.Environ(), "EDITOR=true")
	b, err := sail.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("failed to run %q: %v (out %q)", sail.Args, err, string(b))
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	p.refreshPort()
}

func (p *proxy) proxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second*45)
	defer cancel()

	// Wait until the port refresh goroutine is done.
	for {
		if atomic.LoadInt64(&p.refreshing) == 0 {
			break
		}
		time.Sleep(time.Second)

		if ctx.Err() != nil {
			msg := fmt.Sprintf(`failed to get code server port
taking too long to refresh

please try to reload soon
`)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
	}

	port, portErr := p.getCodeServerPort()
	if portErr != nil {
		msg := fmt.Sprintf(`failed to get code server port
%v

please try to reload soon
`, portErr)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	codeServerProxy(w, r, port)
}

func proxycmd(cntName string) (addr string, err error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", xerrors.Errorf("failed to listen: %w", err)
	}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()

	p := &proxy{
		url:     "http://" + l.Addr().String(),
		cntName: cntName,
	}
	go p.refreshPort()
	go p.gc()

	go func() {
		m := http.NewServeMux()
		m.HandleFunc("/sail.js", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`
console.log("injected")
`))
		})
		m.HandleFunc("/sail/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok\n"))
		})
		m.HandleFunc("/sail/api/v1/reload", p.reload)
		m.HandleFunc("/", p.proxy)
		http.Serve(l, m)
	}()

	return p.url, nil
}
