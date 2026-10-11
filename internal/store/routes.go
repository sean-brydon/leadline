package store

import (
	"context"
	"encoding/json"
	"sort"
)

// A framework's plugin can send the app's whole route list: every page and
// handler the project has, whether or not anything has asked for it yet.
// It comes as a log record (see ingest.RouteLists) and is kept here, the
// newest per service, instead of among the logs. With it the app can show
// the routes nothing has hit.
const routesSchema = `
CREATE TABLE IF NOT EXISTS route_lists (
	service VARCHAR, ts_ns BIGINT, body VARCHAR
);
`

// Route is one route of an app, as its framework names it.
type Route struct {
	// Route is the pattern, as in "/products/[id]": what a request's
	// http.route says.
	Route string `json:"route"`
	// Kind is "page", "route", "api", "layout", "metadata" or "proxy".
	Kind    string   `json:"kind"`
	Router  string   `json:"router,omitempty"`
	File    string   `json:"file,omitempty"`
	Dynamic bool     `json:"dynamic,omitempty"`
	Methods []string `json:"methods,omitempty"`
	Layouts []string `json:"layouts,omitempty"`
	// Requests is how many requests the route had in the window asked
	// about, and LastMs when the newest began (0 for none).
	Requests int64   `json:"requests"`
	LastMs   float64 `json:"last_ms"`
}

// RouteList is one service's routes, as last sent.
type RouteList struct {
	Service   string  `json:"service"`
	UpdatedMs float64 `json:"updated_ms"`
	// Framework is "next"; Version is the framework's own.
	Framework string  `json:"framework"`
	Version   string  `json:"version,omitempty"`
	Root      string  `json:"root,omitempty"`
	Routes    []Route `json:"routes"`
}

// routeBody is the record's body as the plugin writes it.
type routeBody struct {
	Framework string  `json:"framework"`
	Next      string  `json:"next"`
	Root      string  `json:"root"`
	Routes    []Route `json:"routes"`
}

// maxRouteList is the largest list kept, in bytes: a big app has a few
// thousand routes at a hundred-odd bytes each.
const maxRouteList = 4 << 20

// SetRoutes keeps body as service's route list, unless a newer one is
// already kept. body is the JSON the plugin sent; one that can't be read
// is refused, so a bad record never wipes a good list.
func (s *Store) SetRoutes(ctx context.Context, service string, tsNs int64, body string) error {
	var parsed routeBody
	if len(body) > maxRouteList || json.Unmarshal([]byte(body), &parsed) != nil {
		return nil
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var newer int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM route_lists WHERE service = ? AND ts_ns > ?`, service, tsNs).Scan(&newer); err != nil {
		return err
	}
	if newer > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_lists WHERE service = ?`, service); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO route_lists VALUES (?, ?, ?)`, service, tsNs, body); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.gen.Add(1)
	return nil
}

// Routes returns the route lists Leadline holds, by service, with how many
// requests each route had in the window. An empty w.Service means every
// service; a window with no length counts nothing.
func (s *Store) Routes(ctx context.Context, w Window) ([]RouteList, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT service, ts_ns, body FROM route_lists WHERE (? = '' OR service = ?) ORDER BY service`, w.Service, w.Service)
	if err != nil {
		return nil, err
	}
	out := []RouteList{}
	for rows.Next() {
		var l RouteList
		var ts int64
		var body string
		if err := rows.Scan(&l.Service, &ts, &body); err != nil {
			rows.Close()
			return nil, err
		}
		var parsed routeBody
		if json.Unmarshal([]byte(body), &parsed) != nil {
			continue
		}
		l.UpdatedMs = float64(ts) / 1e6
		l.Framework, l.Version, l.Root, l.Routes = parsed.Framework, parsed.Next, parsed.Root, parsed.Routes
		if l.Routes == nil {
			l.Routes = []Route{}
		}
		out = append(out, l)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) == 0 || w.UntilMs <= w.SinceMs {
		return out, nil
	}

	// What was asked for, by the route each request says it matched.
	type key struct{ service, route string }
	type hit struct {
		n    int64
		last float64
	}
	hits := map[key]hit{}
	hrows, err := s.db.QueryContext(ctx, `
SELECT service, http_route, count(*), max(start_ns) / 1e6 FROM spans
WHERE `+inWindow+` AND `+isEntry+` AND http_route <> ''
GROUP BY service, http_route`, w.args()...)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var k key
		var h hit
		if err := hrows.Scan(&k.service, &k.route, &h.n, &h.last); err != nil {
			return nil, err
		}
		hits[k] = h
	}
	if err := hrows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		for j := range out[i].Routes {
			r := &out[i].Routes[j]
			h := hits[key{out[i].Service, r.Route}]
			r.Requests, r.LastMs = h.n, h.last
		}
	}
	return out, nil
}

// QuietRoute is a route that exists and had no request in a window.
type QuietRoute struct {
	Service string   `json:"service"`
	Route   string   `json:"route"`
	Kind    string   `json:"kind"`
	Router  string   `json:"router,omitempty"`
	File    string   `json:"file,omitempty"`
	Dynamic bool     `json:"dynamic,omitempty"`
	Methods []string `json:"methods,omitempty"`
}

// NeverHit lists the routes that had no request in the window, of the
// kinds a request can ask for by name: pages, route handlers and API
// routes. A layout or a proxy runs inside other requests, and is left out.
func (s *Store) NeverHit(ctx context.Context, w Window) ([]QuietRoute, error) {
	lists, err := s.Routes(ctx, w)
	if err != nil {
		return nil, err
	}
	out := []QuietRoute{}
	for _, l := range lists {
		for _, r := range l.Routes {
			if r.Requests > 0 || (r.Kind != "page" && r.Kind != "route" && r.Kind != "api") {
				continue
			}
			out = append(out, QuietRoute{Service: l.Service, Route: r.Route, Kind: r.Kind, Router: r.Router,
				File: r.File, Dynamic: r.Dynamic, Methods: r.Methods})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}
