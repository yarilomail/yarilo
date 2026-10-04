package backendapi

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// routedHeader marks a request another backend-api forwarded here: it runs
// where it landed, so a disagreement between two directors cannot loop it.
const routedHeader = "X-Yarilo-Routed"

// UserRouter says where a user lives. Lookup answers the backend's IP; an
// error means the director could not be asked.
type UserRouter interface {
	Lookup(user string) (string, error)
}

// directorRouter asks the director over its lookup protocol, as the login
// proxies do, in this backend's routing pool.
type directorRouter struct {
	addr string
	tag  string
	tls  *tls.Config
}

// NewDirectorRouter routes by the director at addr; nil when addr is empty,
// which is a deployment without one, where every call runs where it lands.
func NewDirectorRouter(addr, tag string, tlsCfg *tls.Config) UserRouter {
	if addr == "" {
		return nil
	}
	return &directorRouter{addr: addr, tag: tag, tls: tlsCfg}
}

func (d *directorRouter) Lookup(user string) (string, error) {
	var c *proto.Conn
	var err error
	if d.tls != nil {
		c, err = proto.DialTLS(d.addr, "", 0, d.tls)
	} else {
		c, err = proto.Dial(d.addr, "", 0)
	}
	if err != nil {
		return "", fmt.Errorf("backendapi/route: director dial: %w", err)
	}
	defer c.Close() //nolint:errcheck
	res, err := c.Lookup(locks.NewID(), user, d.tag, "imap")
	if err != nil {
		return "", fmt.Errorf("backendapi/route: director lookup: %w", err)
	}
	host, _, err := net.SplitHostPort(res.Addr)
	if err != nil {
		host = res.Addr
	}
	return host, nil
}

// requestUser is the user a request addresses: ?user= or a JSON body's
// "user". The body is put back for the handler.
func requestUser(r *http.Request) string {
	if u := r.URL.Query().Get("user"); u != "" {
		return u
	}
	if r.Body == nil || r.ContentLength == 0 {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close() //nolint:errcheck
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	var probe struct {
		User string `json:"user"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.User
}

// routeUser runs a request about a user where the director keeps them, as the
// reference proxies a user's admin command; it answers true when it handled it.
func (s *Server) routeUser(w http.ResponseWriter, r *http.Request) bool {
	if s.opts.Router == nil || r.Header.Get(routedHeader) != "" {
		return false
	}
	user := requestUser(r)
	if user == "" {
		return false
	}
	owner, err := s.opts.Router.Lookup(user)
	if err != nil {
		// Running here would open the user's index on a pod that is not theirs.
		slog.Warn("backendapi: cannot tell where the user lives", "user", user, "err", err)
		apiError(w, "the director could not be asked where "+user+" lives", http.StatusServiceUnavailable)
		return true
	}
	if owner == "" || owner == s.opts.PodIP {
		return false
	}
	s.forward(w, r, owner)
	return true
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, owner string) {
	scheme := "http"
	if s.opts.PeerTLS != nil {
		scheme = "https"
	}
	target := &url.URL{Scheme: scheme, Host: net.JoinHostPort(owner, s.opts.PeerPort)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if s.opts.PeerTLS != nil {
		proxy.Transport = &http.Transport{TLSClientConfig: s.opts.PeerTLS}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		apiError(w, "the backend the user lives on did not answer: "+err.Error(), http.StatusBadGateway)
	}
	r.Header.Set(routedHeader, s.opts.PodIP)
	proxy.ServeHTTP(w, r)
}
