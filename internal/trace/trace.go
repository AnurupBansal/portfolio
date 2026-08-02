// Package trace reports what the origin observed about an inbound request.
//
// The browser can see its own leg of the journey (via /cdn-cgi/trace and the
// Resource Timing API) but not what happened past Cloudflare. This fills in
// the rest: which edge colo forwarded the request, what protocol Caddy used to
// reach the app, and how long the app itself took.
//
// Deliberately absent: the client IP. Cloudflare hands it over in
// CF-Connecting-IP, and echoing a visitor's own IP back at them is a party
// trick that reads as creepy. Colo and country are enough.
package trace

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Build carries the ldflags-injected identity of this binary.
type Build struct {
	Version   string
	Commit    string
	BuildTime string
	Region    string
}

// Info is the JSON payload. Field names are snake_case to match /api/health.
type Info struct {
	// The inbound request, as the origin saw it.
	Edge     string  `json:"edge"`      // Cloudflare colo (IATA), "" if direct
	EdgeCity string  `json:"edge_city"` // where that colo is, "" if unknown
	EdgeLat  float64 `json:"edge_lat"`  // colo latitude, 0 if unknown
	EdgeLon  float64 `json:"edge_lon"`  // colo longitude, 0 if unknown
	Country  string  `json:"country"`   // CF-IPCountry, "" if direct
	Scheme   string  `json:"scheme"`    // what the browser spoke to Caddy
	AppProto string  `json:"app_proto"` // what Caddy spoke to this process
	ViaEdge  bool    `json:"via_edge"`  // false when Cloudflare is bypassed

	// This process / the origin box.
	Region     string  `json:"region"`
	OriginCity string  `json:"origin_city"`
	OriginLat  float64 `json:"origin_lat"`
	OriginLon  float64 `json:"origin_lon"`
	// Great-circle distance of the public-internet leg, edge to origin, in km.
	// 0 when the request didn't come through a known edge — there's no leg to
	// measure, not a leg of length zero.
	DistanceKm float64 `json:"distance_km"`

	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Go        string `json:"go"`
	UptimeSec int64  `json:"uptime_sec"`

	// Server-side handling cost, in milliseconds. Mirrors the Server-Timing
	// header for callers that would rather read the body.
	AppMs float64 `json:"app_ms"`
}

// Handler returns the /api/trace handler. started should be the process start
// time, shared with /api/health so the two never disagree about uptime.
func Handler(started time.Time, b Build) http.HandlerFunc {
	region := b.Region
	if region == "" {
		region = "unknown"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()

		ray := r.Header.Get("CF-Ray")
		info := Info{
			Edge:       colo(ray),
			Country:    r.Header.Get("CF-IPCountry"),
			Scheme:     scheme(r),
			AppProto:   r.Proto,
			ViaEdge:    ray != "",
			Region:     region,
			Version:    b.Version,
			Commit:     b.Commit,
			BuildTime:  b.BuildTime,
			Go:         runtime.Version(),
			UptimeSec:  int64(time.Since(started).Seconds()),
			OriginCity: originCity,
			OriginLat:  originLat,
			OriginLon:  originLon,
		}

		// Locate the edge and measure the public-internet leg to the origin.
		// Only possible when the colo is one we know the coordinates of; an
		// unknown or absent colo leaves the geo fields at their zero values,
		// which the frontend reads as "no leg to draw".
		if site, ok := coloSites[info.Edge]; ok {
			info.EdgeCity = site.City
			info.EdgeLat = site.Lat
			info.EdgeLon = site.Lon
			info.DistanceKm = math.Round(haversineKm(site.Lat, site.Lon, originLat, originLon))
		}

		// Measured here rather than deferred: both the Server-Timing header and
		// the body have to be written after this point, so JSON encoding and the
		// socket write are necessarily excluded. This is the handler's own cost,
		// not the full request cost — the browser measures that end to end.
		info.AppMs = float64(time.Since(t0).Microseconds()) / 1000

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.3f", info.AppMs))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(info)
	}
}

// colo extracts the three-letter IATA code from a CF-Ray value, which looks
// like "8f3a1b2c3d4e5f6g-BOM". Returns "" for a malformed or absent ray, which
// is the honest answer when the request didn't come through Cloudflare.
func colo(ray string) string {
	i := strings.LastIndexByte(ray, '-')
	if i < 0 {
		return ""
	}
	code := ray[i+1:]
	if len(code) != 3 {
		return ""
	}
	for j := 0; j < len(code); j++ {
		if code[j] < 'A' || code[j] > 'Z' {
			return ""
		}
	}
	return code
}

// scheme reports what the client spoke to the edge of our infrastructure.
// Caddy sets X-Forwarded-Proto; r.TLS is always nil here because Caddy
// terminates TLS and talks plaintext to this process over the Docker network.
func scheme(r *http.Request) string {
	if s := r.Header.Get("X-Forwarded-Proto"); s != "" {
		return s
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// The origin is a single box in Mumbai (ap-south-1) — a physical fact, not a
// guess; it's the "provisioned in Mumbai" line on the build ledger. Coordinates
// are the city, not the rack.
const (
	originCity = "Mumbai"
	originLat  = 19.0760
	originLon  = 72.8777
)

// coloSite is where a Cloudflare edge lives. This is static reference data —
// airport/colo coordinates — not per-request telemetry. The per-request fact is
// *which* colo served you (from CF-Ray); this table only says where that is, so
// the browser can draw the leg. Not exhaustive: an unlisted colo just renders
// without coordinates rather than with a wrong guess.
type coloSite struct {
	City string
	Lat  float64
	Lon  float64
}

var coloSites = map[string]coloSite{
	// India (nearest to the origin)
	"BOM": {"Mumbai", 19.09, 72.87},
	"DEL": {"Delhi", 28.56, 77.10},
	"MAA": {"Chennai", 12.99, 80.17},
	"BLR": {"Bengaluru", 13.20, 77.71},
	"HYD": {"Hyderabad", 17.24, 78.43},
	"CCU": {"Kolkata", 22.65, 88.45},
	// Asia-Pacific
	"SIN": {"Singapore", 1.36, 103.99},
	"HKG": {"Hong Kong", 22.31, 113.91},
	"NRT": {"Tokyo", 35.76, 140.39},
	"KIX": {"Osaka", 34.43, 135.24},
	"ICN": {"Seoul", 37.46, 126.44},
	"KUL": {"Kuala Lumpur", 2.75, 101.71},
	"BKK": {"Bangkok", 13.69, 100.75},
	"CGK": {"Jakarta", -6.13, 106.66},
	"SYD": {"Sydney", -33.94, 151.18},
	"MEL": {"Melbourne", -37.67, 144.84},
	"AKL": {"Auckland", -37.01, 174.79},
	// Middle East & Africa
	"DXB": {"Dubai", 25.25, 55.36},
	"TLV": {"Tel Aviv", 32.01, 34.89},
	"JNB": {"Johannesburg", -26.13, 28.24},
	"CAI": {"Cairo", 30.11, 31.40},
	"IST": {"Istanbul", 41.28, 28.75},
	// Europe
	"LHR": {"London", 51.47, -0.45},
	"CDG": {"Paris", 49.01, 2.55},
	"FRA": {"Frankfurt", 50.03, 8.56},
	"AMS": {"Amsterdam", 52.31, 4.76},
	"MAD": {"Madrid", 40.47, -3.56},
	"MXP": {"Milan", 45.63, 8.72},
	"DUB": {"Dublin", 53.42, -6.27},
	"ARN": {"Stockholm", 59.65, 17.92},
	"WAW": {"Warsaw", 52.17, 20.97},
	"VIE": {"Vienna", 48.11, 16.57},
	"ZRH": {"Zurich", 47.46, 8.55},
	"CPH": {"Copenhagen", 55.62, 12.65},
	"LIS": {"Lisbon", 38.77, -9.13},
	// North America
	"IAD": {"Washington", 38.95, -77.46},
	"EWR": {"Newark", 40.69, -74.17},
	"ATL": {"Atlanta", 33.64, -84.43},
	"ORD": {"Chicago", 41.98, -87.90},
	"DFW": {"Dallas", 32.90, -97.04},
	"MIA": {"Miami", 25.80, -80.29},
	"SEA": {"Seattle", 47.45, -122.31},
	"SJC": {"San Jose", 37.36, -121.93},
	"LAX": {"Los Angeles", 33.94, -118.41},
	"YYZ": {"Toronto", 43.68, -79.61},
	"YVR": {"Vancouver", 49.19, -123.18},
	// South America
	"GRU": {"São Paulo", -23.43, -46.47},
	"EZE": {"Buenos Aires", -34.82, -58.54},
	"SCL": {"Santiago", -33.39, -70.79},
}

// haversineKm returns the great-circle distance in kilometres between two
// points. Used only for the edge→origin leg, where both endpoints are known
// precisely — never for the visitor, whose location we only know to the country.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKm = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(a))
}
