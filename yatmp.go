package yatmp

import (
	"context"
	"encoding/json"
	"log"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	InformUrl      string        `yaml:"informUrl"`
	InformInterval time.Duration `yaml:"informInterval"`
	InformTimeout  time.Duration `yaml:"informTimeout"`
}

type Host struct {
	Regex    string
	AllowIps []string
}

type yatmp struct {
	name   string
	next   http.Handler
	config *Config
	hosts  []Host
	mu     sync.RWMutex
}

type ResponseWriter struct {
	http.ResponseWriter
}

var StatusCode int

func CreateConfig() *Config {
	return &Config{
		InformInterval: 60,
		InformTimeout:  5,
	}
}

// Inform if there are hosts in maintenance
func (a *yatmp) Inform(config *Config) {
	ticker := time.NewTicker(time.Second * config.InformInterval)
	defer ticker.Stop()

	client := &http.Client{
		Timeout: time.Second * config.InformTimeout,
	}

	for range ticker.C {
		req, err := http.NewRequest(http.MethodGet, config.InformUrl, nil)
		if err != nil {
			log.Printf("Inform: %v", err)
			continue
		}

		res, doErr := client.Do(req)
		if doErr != nil {
			log.Printf("Inform: %v", doErr)
			continue
		}

		defer res.Body.Close()
		StatusCode = res.StatusCode

		if StatusCode != 404 {
			var hosts []Host
			decoder := json.NewDecoder(res.Body)
			if decodeErr := decoder.Decode(&hosts); decodeErr != nil {
				log.Printf("Inform: %v", decodeErr)
				continue
			}

			// Safely update the hosts using mutex
			a.mu.Lock()
			a.hosts = hosts
			a.mu.Unlock()
		}
	}
}

// Get all the client's IPs, limit to a reasonable number
func GetClientIps(req *http.Request) []string {
	var ips []string

	if req.RemoteAddr != "" {
		ip, _, splitErr := net.SplitHostPort(req.RemoteAddr)
		if splitErr != nil {
			ip = req.RemoteAddr
		}
		ips = append(ips, ip)
	}

	// Limit the number of forwarded IPs to prevent abuse
	forwardedFor := req.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		for _, ip := range strings.Split(forwardedFor, ",") {
			if len(ips) >= 10 {
				break // Prevent appending more than 10 IPs
			}
			ips = append(ips, strings.TrimSpace(ip))
		}
	}

	return ips
}

// Check if one of the client's IPs has access
func CheckIpAllowed(req *http.Request, host Host) bool {
	for _, ip := range GetClientIps(req) {
		for _, allowIp := range host.AllowIps {
			if ip == allowIp {
				return true
			}
		}
	}
	return false
}

// Check if the host is under maintenance
func (a *yatmp) CheckIfMaintenance(req *http.Request) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, host := range a.hosts {
		if matched, _ := regexp.Match(host.Regex, []byte(req.Host)); matched {
			return !CheckIpAllowed(req, host)
		}
	}

	return false
}

func (rw *ResponseWriter) Header() http.Header {
	return rw.ResponseWriter.Header()
}

func (rw *ResponseWriter) Write(bytes []byte) (int, error) {
	// Avoid buffering, write directly to the client
	return rw.ResponseWriter.Write(bytes)
}

func (rw *ResponseWriter) WriteHeader(statusCode int) {
	rw.ResponseWriter.Header().Del("Last-Modified")
	rw.ResponseWriter.Header().Del("Content-Length")

	rw.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
}

func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	y := &yatmp{
		name:   name,
		next:   next,
		config: config,
	}

	go y.Inform(config)

	return y, nil
}

func (a *yatmp) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if StatusCode != 404 {
		if a.CheckIfMaintenance(req) {
			wrappedWriter := &ResponseWriter{
				ResponseWriter: rw,
			}

			a.next.ServeHTTP(wrappedWriter, req)

			bytes := []byte{}
			contentType := wrappedWriter.Header().Get("Content-Type")
			if contentType != "" {
				mt, _, _ := mime.ParseMediaType(contentType)
				bytes = getTemplate(mt)
			}

			rw.Write(bytes)

			if flusher, ok := rw.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
	}
	a.next.ServeHTTP(rw, req)
}

var templateCache = map[string][]byte{
	"text/html": []byte(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Under maintenance</title>
	<script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="text-center grid place-items-center h-screen">
	<div>
	<h1 class="text-3xl font-bold mb-2">
		This page is under maintenance
	</h1>
	<p>Please come back later.</p>
	</div>
</body>
</html>`),
	"text/plain":       []byte("This page is under maintenance. Please come back later."),
	"application/json": []byte("{\"message\": \"This page is under maintenance. Please come back later.\"}"),
}

// Maintenance page templates with caching
func getTemplate(mediaType string) []byte {
	if tmpl, exists := templateCache[mediaType]; exists {
		return tmpl
	}
	return []byte{}
}
