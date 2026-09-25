package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard stops two ways a page in someone's browser could reach the wall.
//
// The wall runs with the viewer's kubeconfig, and with -allow-button it can
// scale a deployment. Binding to loopback keeps other machines out, but not
// other websites: any page open in the same browser can POST to
// localhost:8099, and a DNS-rebinding page can read from it. So:
//
//   - When the wall listens on loopback, the Host header must be a loopback
//     name too. A rebinding attack arrives with the attacker's hostname.
//   - A request that changes anything must come from the wall's own pages.
//     Browsers always send Origin on a cross-site POST, so a present Origin
//     that is not this host is refused.
func guard(listen string, next http.Handler) http.Handler {
	loopbackOnly := isLoopbackListen(listen)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopbackOnly && !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	return isLoopbackName(host)
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return isLoopbackName(strings.Trim(host, "[]"))
}

func isLoopbackName(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// displayURL is the address to print for people to open.
func displayURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" || isLoopbackName(host) {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}
