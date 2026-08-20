// Package influxread reads telemetry back out of InfluxDB — the read
// side of the pipeline Telegraf writes (see
// packaging/server/usr/lib/farsight/server-netconfig.sh): every metric
// under a telemetry.Payload's "metrics" object becomes its own InfluxDB
// field (Telegraf's json_v2 with disable_prepend_keys), tagged with
// device_id and, when present, record_id. Nothing in this package knows
// about tenants — device_id/record_id scoping is enforced by the caller
// (see cmd/farsight-server's LLM tools handler, which resolves a
// device_id against the caller's tenant via internal/store before ever
// calling in here) — see docs/LLM-INTEGRATION.md.
package influxread

import (
	"context"
	"fmt"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

const measurement = "telemetry"

// lookback bounds schema-discovery queries (FieldKeys, RecentRecordIDs) —
// how far back to look for "what does this device publish" without the
// caller having to specify a range for what's meant to be a cheap,
// approximate discovery query, not a precise historical one.
const lookback = "-90d"

// Every query below does `|> group()` right after filtering, collapsing
// everything into one logical table regardless of tags that aren't part
// of the filter — found necessary against real data: Telegraf's
// mqtt_consumer input tags every point with host and topic in addition
// to device_id/record_id (see server-netconfig.sh), so two points with
// the same device_id+field can still land in different Flux tables.
// Without this, count()/aggregates/distinct() silently operate per-table
// instead of across the whole matching series — undercounting, and a
// concatenation of unsorted per-table rows instead of one real series.

type Client struct {
	queryAPI api.QueryAPI
}

// New creates a client for InfluxDB at url (Telegraf/Grafana both point
// at the same instance — see provisionGrafanaOrg in cmd/farsight-server),
// org and bucket matching the fixed values server-netconfig.sh's Telegraf
// config and provisionGrafanaOrg's datasource already use.
func New(url, token, org, bucket string) *Client {
	c := influxdb2.NewClient(url, token)
	return &Client{queryAPI: c.QueryAPI(org)}
}

// FieldKeys returns every distinct metric name seen for deviceID within
// the lookback window — used by get_device_details so the caller (an LLM)
// knows what it can actually ask get_telemetry_summary/Series for,
// without needing a separate discovery round trip it might forget to
// make (see docs/LLM-INTEGRATION.md "Tool schema v1").
func (c *Client) FieldKeys(ctx context.Context, bucket, deviceID string) ([]string, error) {
	query := fmt.Sprintf(`
		from(bucket: %q)
		|> range(start: %s)
		|> filter(fn: (r) => r._measurement == %q and r.device_id == %q)
		|> group()
		|> keep(columns: ["_field"])
		|> distinct(column: "_field")`,
		bucket, lookback, measurement, deviceID)

	result, err := c.queryAPI.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("influxread: field keys: %w", err)
	}
	defer result.Close()

	var out []string
	for result.Next() {
		if v, ok := result.Record().Value().(string); ok {
			out = append(out, v)
		}
	}
	return out, result.Err()
}

// RecentRecordIDs returns up to limit distinct record_id values seen for
// deviceID within the lookback window — a device with no record_id ever
// set (system telemetry with no per-treatment grouping, see
// internal/telemetry package doc) simply returns an empty slice, not an
// error.
func (c *Client) RecentRecordIDs(ctx context.Context, bucket, deviceID string, limit int) ([]string, error) {
	query := fmt.Sprintf(`
		from(bucket: %q)
		|> range(start: %s)
		|> filter(fn: (r) => r._measurement == %q and r.device_id == %q and exists r.record_id)
		|> group()
		|> keep(columns: ["record_id"])
		|> distinct(column: "record_id")
		|> limit(n: %d)`,
		bucket, lookback, measurement, deviceID, limit)

	result, err := c.queryAPI.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("influxread: recent record ids: %w", err)
	}
	defer result.Close()

	var out []string
	for result.Next() {
		if v, ok := result.Record().Value().(string); ok {
			out = append(out, v)
		}
	}
	return out, result.Err()
}

// Summary is the aggregated shape get_telemetry_summary returns — never
// raw points, see docs/LLM-INTEGRATION.md "Tool response format
// discipline". Count == 0 means no data matched at all; callers should
// render that as an explicit "no data" message, not silently show zeros.
type Summary struct {
	Count  int64
	Min    float64
	Max    float64
	Avg    float64
	Last   float64
	LastAt time.Time
}

func recordIDFilter(recordID string) string {
	if recordID == "" {
		return ""
	}
	return fmt.Sprintf(" and r.record_id == %q", recordID)
}

// Summary computes min/max/mean/count/last for one metric of one device
// over [from, to) — a single Flux query via union() of five differently
// aggregated copies of the same filtered stream, rather than five round
// trips. count()'s toFloat() cast is required, not cosmetic: union()
// rejects a "_value" column that's int in one table and float in
// another, and count() always returns int64 regardless of the source
// field's own type (found by hitting exactly this error against real
// data).
func (c *Client) Summary(ctx context.Context, bucket, deviceID, metric string, from, to time.Time, recordID string) (Summary, error) {
	query := fmt.Sprintf(`
		base = from(bucket: %q)
			|> range(start: %s, stop: %s)
			|> filter(fn: (r) => r._measurement == %q and r.device_id == %q and r._field == %q%s)
			|> group()

		union(tables: [
			base |> min()  |> set(key: "agg", value: "min"),
			base |> max()  |> set(key: "agg", value: "max"),
			base |> mean() |> set(key: "agg", value: "mean"),
			base |> count() |> toFloat() |> set(key: "agg", value: "count"),
			base |> last() |> set(key: "agg", value: "last"),
		])`,
		bucket, rfc3339(from), rfc3339(to), measurement, deviceID, metric, recordIDFilter(recordID))

	result, err := c.queryAPI.Query(ctx, query)
	if err != nil {
		return Summary{}, fmt.Errorf("influxread: summary: %w", err)
	}
	defer result.Close()

	var s Summary
	for result.Next() {
		rec := result.Record()
		agg, _ := rec.ValueByKey("agg").(string)
		val := toFloat(rec.Value())
		switch agg {
		case "min":
			s.Min = val
		case "max":
			s.Max = val
		case "mean":
			s.Avg = val
		case "count":
			s.Count = int64(val)
		case "last":
			s.Last = val
			s.LastAt = rec.Time()
		}
	}
	if err := result.Err(); err != nil {
		return Summary{}, err
	}
	return s, nil
}

// Point is one sample of a telemetry series.
type Point struct {
	Ts    time.Time
	Value float64
}

// Series returns up to maxPoints samples of one metric of one device over
// [from, to) — if the real point count exceeds maxPoints, downsamples via
// Flux aggregateWindow (mean per bucket) rather than truncating to the
// first N, which would make the series unrepresentative of the full
// range (see docs/LLM-INTEGRATION.md). Bucket width is computed from the
// requested [from, to), not from where the data actually falls within
// it — if the real points are clustered in a narrow slice of a much
// wider requested range, the result can end up with fewer than maxPoints
// non-empty buckets. Still correct (never more than maxPoints, never
// unrepresentative), just not always using the full requested
// resolution — acceptable since a caller asking a real question
// typically requests a range matching what it actually wants to see.
func (c *Client) Series(ctx context.Context, bucket, deviceID, metric string, from, to time.Time, recordID string, maxPoints int) (points []Point, totalAvailable int64, truncated bool, err error) {
	filter := fmt.Sprintf(`r._measurement == %q and r.device_id == %q and r._field == %q%s`,
		measurement, deviceID, metric, recordIDFilter(recordID))

	countQuery := fmt.Sprintf(`
		from(bucket: %q)
		|> range(start: %s, stop: %s)
		|> filter(fn: (r) => %s)
		|> group()
		|> count()`,
		bucket, rfc3339(from), rfc3339(to), filter)

	countResult, err := c.queryAPI.Query(ctx, countQuery)
	if err != nil {
		return nil, 0, false, fmt.Errorf("influxread: series count: %w", err)
	}
	if countResult.Next() {
		totalAvailable = int64(toFloat(countResult.Record().Value()))
	}
	countResult.Close()
	if err := countResult.Err(); err != nil {
		return nil, 0, false, err
	}
	if totalAvailable == 0 {
		return nil, 0, false, nil
	}

	var dataQuery string
	if totalAvailable <= int64(maxPoints) {
		dataQuery = fmt.Sprintf(`
			from(bucket: %q)
			|> range(start: %s, stop: %s)
			|> filter(fn: (r) => %s)
			|> group()
			|> sort(columns: ["_time"])`,
			bucket, rfc3339(from), rfc3339(to), filter)
	} else {
		// Bucket width chosen so the number of windows lands at maxPoints —
		// downsampling, not truncation (see doc comment above).
		windowSeconds := int64(to.Sub(from).Seconds()) / int64(maxPoints)
		if windowSeconds < 1 {
			windowSeconds = 1
		}
		truncated = true
		dataQuery = fmt.Sprintf(`
			from(bucket: %q)
			|> range(start: %s, stop: %s)
			|> filter(fn: (r) => %s)
			|> group()
			|> aggregateWindow(every: %ds, fn: mean, createEmpty: false)
			|> sort(columns: ["_time"])`,
			bucket, rfc3339(from), rfc3339(to), filter, windowSeconds)
	}

	result, err := c.queryAPI.Query(ctx, dataQuery)
	if err != nil {
		return nil, 0, false, fmt.Errorf("influxread: series: %w", err)
	}
	defer result.Close()

	for result.Next() {
		rec := result.Record()
		points = append(points, Point{Ts: rec.Time(), Value: toFloat(rec.Value())})
		if len(points) >= maxPoints {
			truncated = true
			break
		}
	}
	if err := result.Err(); err != nil {
		return nil, 0, false, err
	}
	return points, totalAvailable, truncated, nil
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// toFloat handles both int64 and float64 — a field's underlying InfluxDB
// type depends on the first value ever written for it (line protocol
// infers int vs float), aggregates like count()/min()/max() can come
// back as either depending on that, not always float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	default:
		return 0
	}
}
