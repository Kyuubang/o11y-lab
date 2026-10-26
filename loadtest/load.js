// k6 load test for the o11y lab services.
//
//   PROFILE=load   (default) steady traffic, ~3 min
//   PROFILE=stress ramp up until things hurt, ~6 min
//   PROFILE=spike  short burst, ~2 min
//   DURATION=10m   override the steady-state duration for "load"
//
// k6 metrics are also remote-written to VictoriaMetrics (k6_* series) when run
// with `-o experimental-prometheus-rw` (scripts/stress.sh does this).
import http from 'k6/http';
import { check, sleep } from 'k6';

const INVENTORY = __ENV.INVENTORY_URL || 'http://localhost:8081';
const ORDER = __ENV.ORDER_URL || 'http://localhost:8082';
const PROFILE = __ENV.PROFILE || 'load';
const DURATION = __ENV.DURATION || '3m';

const profiles = {
  load: {
    orders: { executor: 'constant-arrival-rate', rate: 20, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 20, maxVUs: 100 },
    inventory: { executor: 'constant-vus', vus: 5, duration: DURATION },
  },
  stress: {
    orders: {
      executor: 'ramping-arrival-rate', startRate: 10, timeUnit: '1s', preAllocatedVUs: 50, maxVUs: 400,
      stages: [
        { target: 50, duration: '1m' },
        { target: 150, duration: '2m' },
        { target: 300, duration: '2m' },
        { target: 0, duration: '1m' },
      ],
    },
    inventory: {
      executor: 'ramping-vus', startVUs: 2,
      stages: [
        { target: 10, duration: '1m' },
        { target: 40, duration: '4m' },
        { target: 0, duration: '1m' },
      ],
    },
  },
  spike: {
    orders: {
      executor: 'ramping-arrival-rate', startRate: 5, timeUnit: '1s', preAllocatedVUs: 50, maxVUs: 500,
      stages: [
        { target: 5, duration: '30s' },
        { target: 400, duration: '10s' },
        { target: 400, duration: '30s' },
        { target: 5, duration: '10s' },
        { target: 5, duration: '30s' },
      ],
    },
    inventory: { executor: 'constant-vus', vus: 5, duration: '110s' },
  },
};

const selected = profiles[PROFILE];
if (!selected) throw new Error(`unknown PROFILE ${PROFILE}`);

export const options = {
  scenarios: {
    orders: { ...selected.orders, exec: 'orders' },
    inventory: { ...selected.inventory, exec: 'inventory' },
    restock: { executor: 'constant-arrival-rate', rate: 1, timeUnit: '5s', duration: maxDuration(selected), preAllocatedVUs: 1, exec: 'restock' },
  },
  thresholds: {
    'http_req_failed{scenario:orders}': ['rate<0.10'],
    'http_req_duration{scenario:orders}': ['p(95)<1000'],
    'http_req_duration{scenario:inventory}': ['p(95)<500'],
  },
};

function maxDuration(p) {
  const toSec = (d) => {
    const m = /^(\d+)(ms|s|m|h)$/.exec(d);
    const mult = { ms: 0.001, s: 1, m: 60, h: 3600 }[m[2]];
    return Number(m[1]) * mult;
  };
  const scenarioSec = (s) => s.duration ? toSec(s.duration) : s.stages.reduce((a, st) => a + toSec(st.duration), 0);
  return `${Math.ceil(Math.max(scenarioSec(p.orders), scenarioSec(p.inventory)))}s`;
}

const json = { headers: { 'Content-Type': 'application/json' } };
const rand = (min, max) => Math.floor(Math.random() * (max - min + 1)) + min;

export function setup() {
  const res = http.get(`${INVENTORY}/items`);
  const ok = check(res, { 'setup: items listed': (r) => r.status === 200 });
  if (!ok) throw new Error(`inventory-api not ready: ${res.status}`);
  const ids = res.json().map((i) => i.id);
  if (ids.length === 0) throw new Error('no items seeded');
  return { ids };
}

// Order flow: order-api -> inventory-api (GET item, POST reserve) -> Postgres.
export function orders(data) {
  const itemId = data.ids[rand(0, data.ids.length - 1)];
  const res = http.post(`${ORDER}/orders`, JSON.stringify({ item_id: itemId, quantity: rand(1, 3) }), {
    ...json, tags: { name: 'POST /orders' },
  });
  check(res, { 'order: created or out-of-stock': (r) => r.status === 201 || r.status === 409 });

  if (Math.random() < 0.2) {
    http.get(`${ORDER}/catalog`, { tags: { name: 'GET /catalog' } });
  }
  if (res.status === 201 && Math.random() < 0.3) {
    http.get(`${ORDER}/orders/${res.json().id}`, { tags: { name: 'GET /orders/{id}' } });
  }
}

// Inventory CRUD: create -> read -> update -> list -> delete, plus a 404.
export function inventory() {
  const sku = `LT-${__VU}-${__ITER}-${Date.now()}`;
  const created = http.post(`${INVENTORY}/items`, JSON.stringify({ sku, name: `Load item ${sku}`, price: rand(1, 500), stock: rand(1, 100) }), {
    ...json, tags: { name: 'POST /items' },
  });
  if (!check(created, { 'item: created': (r) => r.status === 201 })) {
    sleep(1);
    return;
  }
  const id = created.json().id;

  check(http.get(`${INVENTORY}/items/${id}`, { tags: { name: 'GET /items/{id}' } }), { 'item: read': (r) => r.status === 200 });
  check(http.put(`${INVENTORY}/items/${id}`, JSON.stringify({ name: `Updated ${sku}`, price: rand(1, 500), stock: rand(1, 100) }), {
    ...json, tags: { name: 'PUT /items/{id}' },
  }), { 'item: updated': (r) => r.status === 200 });
  check(http.get(`${INVENTORY}/items`, { tags: { name: 'GET /items' } }), { 'item: listed': (r) => r.status === 200 });
  check(http.del(`${INVENTORY}/items/${id}`, null, { tags: { name: 'DELETE /items/{id}' } }), { 'item: deleted': (r) => r.status === 204 });

  http.get(`${INVENTORY}/items/999999999`, { tags: { name: 'GET /items/{id}' }, responseCallback: http.expectedStatuses(404) });
  sleep(Math.random() * 0.5);
}

// Keeps seeded items in stock so the order flow doesn't flatline into 409s.
export function restock(data) {
  for (const id of data.ids) {
    const res = http.get(`${INVENTORY}/items/${id}`, { tags: { name: 'GET /items/{id}' } });
    if (res.status !== 200) continue;
    const item = res.json();
    if (item.stock < 200) {
      http.put(`${INVENTORY}/items/${id}`, JSON.stringify({ name: item.name, price: item.price, stock: 1000 }), {
        ...json, tags: { name: 'PUT /items/{id}' },
      });
    }
  }
}
