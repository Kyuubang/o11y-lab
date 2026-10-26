// inventory-api: CRUD over a Postgres "items" table, instrumented with
// OpenTelemetry (HTTP + database/sql spans and metrics) and JSON logs.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/XSAM/otelsql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"o11ylab/internal/httpx"
	"o11ylab/internal/telemetry"
)

const serviceName = "inventory-api"

type Item struct {
	ID        int64     `json:"id"`
	SKU       string    `json:"sku"`
	Name      string    `json:"name"`
	Price     float64   `json:"price"`
	Stock     int       `json:"stock"`
	UpdatedAt time.Time `json:"updated_at"`
}

type itemInput struct {
	SKU   string  `json:"sku"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

type reserveInput struct {
	Quantity int `json:"quantity"`
}

var errNotFound = errors.New("item not found")

type app struct {
	db       *sql.DB
	log      *slog.Logger
	tracer   trace.Tracer
	reserved metric.Int64Counter
	stockGap metric.Int64Counter
}

func main() {
	log := telemetry.NewLogger(serviceName)
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()
	shutdown, err := telemetry.Setup(ctx, serviceName, telemetry.Env("SERVICE_VERSION", "dev"))
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer shutdown(context.Background())

	db, err := otelsql.Open("pgx", telemetry.Env("DATABASE_URL", "postgres://lab:lab@localhost:5432/inventory?sslmode=disable"),
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithSpanOptions(otelsql.SpanOptions{OmitConnResetSession: true, OmitRows: true}),
	)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	if _, err := otelsql.RegisterDBStatsMetrics(db, otelsql.WithAttributes(semconv.DBSystemPostgreSQL)); err != nil {
		return err
	}

	if err := waitForDB(ctx, db, log); err != nil {
		return err
	}
	if err := migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	meter := otel.Meter(serviceName)
	a := &app{db: db, log: log, tracer: otel.Tracer(serviceName)}
	if a.reserved, err = meter.Int64Counter("inventory.items.reserved",
		metric.WithDescription("Units reserved from stock"), metric.WithUnit("{unit}")); err != nil {
		return err
	}
	if a.stockGap, err = meter.Int64Counter("inventory.reservations.rejected",
		metric.WithDescription("Reservations rejected because of insufficient stock")); err != nil {
		return err
	}

	rt := httpx.NewRouter(log)
	rt.Handle("GET /healthz", a.health)
	rt.Handle("GET /items", a.listItems)
	rt.Handle("POST /items", a.createItem)
	rt.Handle("GET /items/{id}", a.getItem)
	rt.Handle("PUT /items/{id}", a.updateItem)
	rt.Handle("DELETE /items/{id}", a.deleteItem)
	rt.Handle("POST /items/{id}/reserve", a.reserveItem)

	return httpx.Serve(log, ":"+telemetry.Env("PORT", "8080"), rt.Handler(serviceName))
}

func waitForDB(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		log.Warn("waiting for database", "attempt", i+1, "error", err)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("database not reachable: %w", err)
}

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS items (
  id         BIGSERIAL PRIMARY KEY,
  sku        TEXT UNIQUE NOT NULL,
  name       TEXT NOT NULL,
  price      NUMERIC(12,2) NOT NULL DEFAULT 0,
  stock      INTEGER NOT NULL DEFAULT 0 CHECK (stock >= 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO items (sku, name, price, stock) VALUES
  ('SKU-001', 'Mechanical Keyboard', 89.90, 1000),
  ('SKU-002', 'Wireless Mouse',      29.50, 1000),
  ('SKU-003', '27in Monitor',       219.00,  500),
  ('SKU-004', 'USB-C Dock',          99.00,  500),
  ('SKU-005', 'Noise Cancelling Headphones', 149.00, 300)
ON CONFLICT (sku) DO NOTHING;`)
	return err
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		httpx.Error(w, r, http.StatusServiceUnavailable, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) listItems(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, sku, name, price, stock, updated_at FROM items ORDER BY id LIMIT 100`)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.SKU, &it.Name, &it.Price, &it.Stock, &it.UpdatedAt); err != nil {
			httpx.Error(w, r, http.StatusInternalServerError, err)
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (a *app) getItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := a.findItem(r.Context(), id)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, it)
}

func (a *app) createItem(w http.ResponseWriter, r *http.Request) {
	var in itemInput
	if err := httpx.Decode(r, &in); err != nil || in.SKU == "" || in.Name == "" || in.Stock < 0 {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid body: sku, name required; stock >= 0"))
		return
	}
	var it Item
	err := a.db.QueryRowContext(r.Context(),
		`INSERT INTO items (sku, name, price, stock) VALUES ($1, $2, $3, $4)
		 RETURNING id, sku, name, price, stock, updated_at`,
		in.SKU, in.Name, in.Price, in.Stock,
	).Scan(&it.ID, &it.SKU, &it.Name, &it.Price, &it.Stock, &it.UpdatedAt)
	if err != nil {
		httpx.Error(w, r, http.StatusConflict, err)
		return
	}
	a.log.InfoContext(r.Context(), "item created", "item_id", it.ID, "sku", it.SKU)
	httpx.JSON(w, http.StatusCreated, it)
}

func (a *app) updateItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in itemInput
	if err := httpx.Decode(r, &in); err != nil || in.Name == "" || in.Stock < 0 {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid body: name required; stock >= 0"))
		return
	}
	var it Item
	err := a.db.QueryRowContext(r.Context(),
		`UPDATE items SET name=$2, price=$3, stock=$4, updated_at=now() WHERE id=$1
		 RETURNING id, sku, name, price, stock, updated_at`,
		id, in.Name, in.Price, in.Stock,
	).Scan(&it.ID, &it.SKU, &it.Name, &it.Price, &it.Stock, &it.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, it)
}

func (a *app) deleteItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM items WHERE id=$1`, id)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.writeErr(w, r, errNotFound)
		return
	}
	a.log.InfoContext(r.Context(), "item deleted", "item_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// reserveItem decrements stock inside a transaction with a row lock, which
// gives multi-statement DB spans worth looking at in a trace.
func (a *app) reserveItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in reserveInput
	if err := httpx.Decode(r, &in); err != nil || in.Quantity <= 0 {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid body: quantity > 0 required"))
		return
	}

	ctx, span := a.tracer.Start(r.Context(), "inventory.reserve",
		trace.WithAttributes(attribute.Int64("item.id", id), attribute.Int("reserve.quantity", in.Quantity)))
	defer span.End()

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	var it Item
	err = tx.QueryRowContext(ctx,
		`SELECT id, sku, name, price, stock, updated_at FROM items WHERE id=$1 FOR UPDATE`, id,
	).Scan(&it.ID, &it.SKU, &it.Name, &it.Price, &it.Stock, &it.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if it.Stock < in.Quantity {
		a.stockGap.Add(ctx, 1, metric.WithAttributes(attribute.String("sku", it.SKU)))
		a.log.WarnContext(ctx, "insufficient stock", "item_id", id, "stock", it.Stock, "requested", in.Quantity)
		httpx.Error(w, r, http.StatusConflict, fmt.Errorf("insufficient stock: have %d, want %d", it.Stock, in.Quantity))
		return
	}
	err = tx.QueryRowContext(ctx,
		`UPDATE items SET stock = stock - $2, updated_at = now() WHERE id=$1 RETURNING stock, updated_at`,
		id, in.Quantity,
	).Scan(&it.Stock, &it.UpdatedAt)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	a.reserved.Add(ctx, int64(in.Quantity), metric.WithAttributes(attribute.String("sku", it.SKU)))
	a.log.InfoContext(ctx, "stock reserved", "item_id", id, "quantity", in.Quantity, "remaining", it.Stock)
	httpx.JSON(w, http.StatusOK, it)
}

func (a *app) findItem(ctx context.Context, id int64) (Item, error) {
	var it Item
	err := a.db.QueryRowContext(ctx,
		`SELECT id, sku, name, price, stock, updated_at FROM items WHERE id=$1`, id,
	).Scan(&it.ID, &it.SKU, &it.Name, &it.Price, &it.Stock, &it.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return it, errNotFound
	}
	return it, err
}

func (a *app) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotFound) {
		httpx.Error(w, r, http.StatusNotFound, err)
		return
	}
	a.log.ErrorContext(r.Context(), "database error", "error", err)
	httpx.Error(w, r, http.StatusInternalServerError, err)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid id"))
		return 0, false
	}
	return id, true
}
