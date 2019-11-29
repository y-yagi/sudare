package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/transport"
	"github.com/y-yagi/debuglog"
)

type Meta struct {
	req  *http.Request
	resp *http.Response
	err  error
	t    time.Time
	sess int64
	from string
}

var hosts map[int64]string
var debuglogger *debuglog.Logger

func fprintf(nr *int64, err *error, w io.Writer, pat string, a ...interface{}) {
	if *err != nil {
		return
	}
	var n int
	n, *err = fmt.Fprintf(w, pat, a...)
	*nr += int64(n)
}

func write(nr *int64, err *error, w io.Writer, b []byte) {
	if *err != nil {
		return
	}
	var n int
	n, *err = w.Write(b)
	*nr += int64(n)
}

func (m *Meta) WriteTo(w io.Writer) (nr int64, err error) {
	if m.req != nil {
		fprintf(&nr, &err, w, "Type: request\r\n")
	} else if m.resp != nil {
		fprintf(&nr, &err, w, "Type: response\r\n")
	}
	fprintf(&nr, &err, w, "ReceivedAt: %v\r\n", m.t)
	fprintf(&nr, &err, w, "Session: %d\r\n", m.sess)
	fprintf(&nr, &err, w, "From: %v\r\n", m.from)
	fprintf(&nr, &err, w, "Host: %v\r\n", hosts[m.sess-1])

	if m.err != nil {
		// note the empty response
		fprintf(&nr, &err, w, "Error: %v\r\n\r\n\r\n\r\n", m.err)
	} else if m.req != nil {
		fprintf(&nr, &err, w, "\r\n")
		buf, err2 := httputil.DumpRequest(m.req, false)
		if err2 != nil {
			return nr, err2
		}
		write(&nr, &err, w, buf)
	} else if m.resp != nil {
		fprintf(&nr, &err, w, "\r\n")
		buf, err2 := httputil.DumpResponse(m.resp, false)
		if err2 != nil {
			return nr, err2
		}
		write(&nr, &err, w, buf)
	}
	return
}

// HttpLogger is an asynchronous HTTP request/response logger. It traces
// requests and responses headers in a "log" file in logger directory and dumps
// their bodies in files prefixed with the session identifiers.
// Close it to ensure pending items are correctly logged.
type HttpLogger struct {
	c     chan *Meta
	errch chan error
}

func NewLogger(filename string) (*HttpLogger, error) {
	f, err := os.Create(filename)
	if err != nil {
		return nil, err
	}

	logger := &HttpLogger{make(chan *Meta), make(chan error)}
	go func() {
		for m := range logger.c {
			if _, err := m.WriteTo(f); err != nil {
				log.Println("Can't write meta", err)
			}
		}
		logger.errch <- f.Close()
	}()

	return logger, nil
}

func (logger *HttpLogger) LogResp(resp *http.Response, ctx *goproxy.ProxyCtx) {
	from := ""
	if ctx.UserData != nil {
		from = ctx.UserData.(*transport.RoundTripDetails).TCPAddr.String()
	}
	if resp == nil {
		resp = emptyResp
	}

	logger.LogMeta(&Meta{
		resp: resp,
		err:  ctx.Error,
		t:    time.Now(),
		sess: ctx.Session,
		from: from,
	})
}

var emptyResp = &http.Response{}
var emptyReq = &http.Request{}

func (logger *HttpLogger) LogReq(req *http.Request, ctx *goproxy.ProxyCtx) {
	if req == nil {
		req = emptyReq
	}

	logger.LogMeta(&Meta{
		req:  req,
		err:  ctx.Error,
		t:    time.Now(),
		sess: ctx.Session,
		from: req.RemoteAddr,
	})
}

func (logger *HttpLogger) LogMeta(m *Meta) {
	logger.c <- m
}

func (logger *HttpLogger) Close() error {
	close(logger.c)
	return <-logger.errch
}

type stoppableListener struct {
	net.Listener
	sync.WaitGroup
}

type stoppableConn struct {
	net.Conn
	wg *sync.WaitGroup
}

func newStoppableListener(l net.Listener) *stoppableListener {
	return &stoppableListener{l, sync.WaitGroup{}}
}

func (sl *stoppableListener) Accept() (net.Conn, error) {
	c, err := sl.Listener.Accept()
	if err != nil {
		return c, err
	}
	sl.Add(1)
	debuglogger.Printf("[Accept] Add sync.WaitGroup\n")
	return &stoppableConn{c, &sl.WaitGroup}, nil
}

func (sc *stoppableConn) Close() error {
	sc.wg.Done()
	debuglogger.Printf("[Close] Done sync.WaitGroup\n")
	return sc.Conn.Close()
}

func main() {
	verbose := flag.Bool("v", false, "should every proxy request be logged to stdout")
	addr := flag.String("l", ":8080", "on which address should the proxy listen")
	flag.Parse()

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = *verbose
	hosts = map[int64]string{}
	debuglogger = debuglog.New(os.Stdout)

	logger, err := NewLogger("proxy.log")
	if err != nil {
		log.Fatal("can't open log file", err)
	}

	tr := transport.Transport{Proxy: transport.ProxyFromEnvironment}

	// var httpsHanlder goproxy.FuncHttpsHandler = func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	// 	hosts[ctx.Session] = host
	// 	return goproxy.OkConnect, host
	// }
	// proxy.OnRequest().HandleConnect(httpsHanlder)

	proxy.OnRequest(goproxy.UrlMatches(regexp.MustCompile(`.*[jpg|png|svg]$`))).DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			return r, goproxy.NewResponse(r, goproxy.ContentTypeText, http.StatusForbidden, "Don't waste your GIGA!")
		})

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (resp *http.Response, err error) {
			ctx.UserData, resp, err = tr.DetailedRoundTrip(req)
			return
		})
		logger.LogReq(req, ctx)
		return req, nil
	})
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		logger.LogResp(resp, ctx)
		return resp
	})

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal("listen:", err)
	}

	sl := newStoppableListener(l)
	ch := make(chan os.Signal)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		log.Println("Got SIGINT exiting")
		sl.Add(1)
		sl.Close()
		logger.Close()
		sl.Done()
	}()

	log.Println("Starting Proxy. addr:", *addr)
	http.Serve(sl, proxy)
	sl.Wait()
	log.Println("All connections closed - exit")
}
