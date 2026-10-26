// Package httpx holds HTTP helpers shared by the lab services: route-aware
// OpenTelemetry instrumentation, access logging, chaos injection and JSON I/O.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Router wraps http.ServeMux so every server span gets a low-cardinality name
// such as "GET /items/{id}" instead of just "inventory-api".
type Router struct {
	mux   *http.ServeMux
	log   *slog.Logger
	chaos Chaos
}

func NewRouter(log *slog.Logger) *Router {
	return &Router{mux: http.NewServeMux(), log: log, chaos: ChaosFromEnv()}
}

// Handle registers a Go 1.22+ pattern such as "GET /items/{id}".
func (rt *Router) Handle(pattern string, h http.HandlerFunc) {
	rt.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		// otelhttp derives http.route from r.Pattern; we also use it as the span name.
		trace.SpanFromContext(r.Context()).SetName(r.Pattern)

		if pattern != "GET /healthz" && rt.chaos.Inject(w, r) {
			return
		}
		h(w, r)
	})
}

// Handler returns the fully instrumented root handler.
func (rt *Router) Handler(service string) http.Handler {
	return otelhttp.NewHandler(rt.accessLog(rt.mux), service)
}

func (rt *Router) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		rt.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"route", r.Pattern,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Chaos injects random latency and 5xx errors so dashboards have something
// interesting to show. Controlled by CHAOS_ERROR_RATE (0..1) and
// CHAOS_MAX_LATENCY_MS.
type Chaos struct {
	ErrorRate    float64
	MaxLatencyMS int
}

func ChaosFromEnv() Chaos {
	c := Chaos{}
	c.ErrorRate, _ = strconv.ParseFloat(os.Getenv("CHAOS_ERROR_RATE"), 64)
	c.MaxLatencyMS, _ = strconv.Atoi(os.Getenv("CHAOS_MAX_LATENCY_MS"))
	return c
}

func (c Chaos) Inject(w http.ResponseWriter, r *http.Request) bool {
	if c.MaxLatencyMS > 0 {
		time.Sleep(time.Duration(rand.IntN(c.MaxLatencyMS)) * time.Millisecond)
	}
	if c.ErrorRate > 0 && rand.Float64() < c.ErrorRate {
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attribute.Bool("chaos.injected", true))
		Error(w, r, http.StatusServiceUnavailable, errors.New("chaos: injected failure"))
		return true
	}
	return false
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes a JSON error and marks the active span as failed for 5xx.
func Error(w http.ResponseWriter, r *http.Request, status int, err error) {
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	if status >= 500 {
		span.SetStatus(codes.Error, err.Error())
	}
	JSON(w, status, map[string]string{"error": err.Error()})
}

func Decode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Serve runs the server until SIGINT/SIGTERM, then shuts down gracefully.
func Serve(log *slog.Logger, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
