package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// SensorRepository stores node hardware readings and what is known about
// reaching each node (ADR 0007).
type SensorRepository struct{ db *Pool }

// NewSensorRepository builds the repository.
func NewSensorRepository(db *Pool) *SensorRepository { return &SensorRepository{db: db} }

// Write inserts one node's readings. A pass over a handful of nodes is tens of
// rows, so COPY keeps it one round trip like the metrics writer.
func (r *SensorRepository) Write(ctx context.Context, in ports.SensorReadings) (int, error) {
	if len(in.Readings) == 0 {
		return 0, nil
	}
	n, err := r.db.CopyFrom(ctx,
		pgx.Identifier{"host_sensors"},
		[]string{"time", "host_id", "chip", "label", "kind", "value", "high", "crit"},
		pgx.CopyFromSlice(len(in.Readings), func(i int) ([]any, error) {
			s := in.Readings[i]
			return []any{in.At, in.HostID, s.Chip, s.Label, string(s.Kind), s.Value, s.High, s.Crit}, nil
		}))
	if err != nil {
		return 0, fmt.Errorf("write sensor readings: %w", err)
	}
	return int(n), nil
}

// latestReadings is the shared shape: the most recent row per (chip, label)
// for the hosts asked about. DISTINCT ON is the cheap way to say that in
// Postgres, and the (host_id, chip, label, time DESC) index serves it directly.
const latestReadings = `
SELECT DISTINCT ON (host_id, chip, label)
       host_id, time, chip, label, kind, value, high, crit
FROM host_sensors
WHERE host_id = ANY($1) AND time > $2
ORDER BY host_id, chip, label, time DESC`

// staleAfter is how old the newest reading may be before a node counts as
// silent. Three collection intervals: one missed poll is a blip, three is a
// node that has stopped answering.
const staleAfter = 15 * time.Minute

// Latest returns every sensor's most recent reading for one node.
func (r *SensorRepository) Latest(ctx context.Context, hostID uuid.UUID) (ports.SensorReadings, error) {
	out := ports.SensorReadings{HostID: hostID}
	rows, err := r.db.Query(ctx, latestReadings, []uuid.UUID{hostID}, time.Now().Add(-staleAfter))
	if err != nil {
		return out, fmt.Errorf("latest sensors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		reading, at, _, err := scanReading(rows)
		if err != nil {
			return out, err
		}
		if at.After(out.At) {
			out.At = at
		}
		out.Readings = append(out.Readings, reading)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("latest sensors: %w", err)
	}
	telemetry.SortReadings(out.Readings)
	return out, nil
}

// Summaries reduces each host's readings to what a list row shows, in one
// query rather than one per row.
func (r *SensorRepository) Summaries(ctx context.Context, hostIDs []uuid.UUID) (map[uuid.UUID]telemetry.SensorSummary, error) {
	out := map[uuid.UUID]telemetry.SensorSummary{}
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, latestReadings, hostIDs, time.Now().Add(-staleAfter))
	if err != nil {
		return nil, fmt.Errorf("sensor summaries: %w", err)
	}
	defer rows.Close()

	byHost := map[uuid.UUID][]telemetry.Reading{}
	for rows.Next() {
		reading, _, hostID, err := scanReading(rows)
		if err != nil {
			return nil, err
		}
		byHost[hostID] = append(byHost[hostID], reading)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sensor summaries: %w", err)
	}
	for hostID, readings := range byHost {
		out[hostID] = telemetry.Summarize(readings)
	}
	return out, nil
}

// HottestNow is what the alert evaluator reads: one reading per host, the one
// nearest its own limit.
func (r *SensorRepository) HottestNow(ctx context.Context, since time.Time) (map[uuid.UUID]telemetry.Reading, error) {
	rows, err := r.db.Query(ctx, `
SELECT DISTINCT ON (host_id, chip, label)
       host_id, time, chip, label, kind, value, high, crit
FROM host_sensors
WHERE time > $1 AND kind = 'temp_c'
ORDER BY host_id, chip, label, time DESC`, since)
	if err != nil {
		return nil, fmt.Errorf("hottest sensors: %w", err)
	}
	defer rows.Close()

	byHost := map[uuid.UUID][]telemetry.Reading{}
	for rows.Next() {
		reading, _, hostID, err := scanReading(rows)
		if err != nil {
			return nil, err
		}
		byHost[hostID] = append(byHost[hostID], reading)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hottest sensors: %w", err)
	}

	out := make(map[uuid.UUID]telemetry.Reading, len(byHost))
	for hostID, readings := range byHost {
		if hottest, ok := telemetry.Hottest(readings); ok {
			out[hostID] = hottest
		}
	}
	return out, nil
}

// Series is one sensor's history. Raw rows inside their 48-hour retention,
// the five-minute rollup beyond it — the same trade the metric charts make.
func (r *SensorRepository) Series(ctx context.Context, hostID uuid.UUID, chip, label string,
	from, to time.Time, res telemetry.Resolution) ([]ports.SensorPoint, error) {
	query := `
SELECT time, value, value
FROM host_sensors
WHERE host_id = $1 AND chip = $2 AND label = $3 AND time >= $4 AND time <= $5
ORDER BY time`
	if res != telemetry.ResolutionRaw {
		query = `
SELECT bucket, value_avg, value_max
FROM host_sensors_5m
WHERE host_id = $1 AND chip = $2 AND label = $3 AND bucket >= $4 AND bucket <= $5
ORDER BY bucket`
	}

	rows, err := r.db.Query(ctx, query, hostID, chip, label, from, to)
	if err != nil {
		return nil, fmt.Errorf("sensor series: %w", err)
	}
	defer rows.Close()

	var out []ports.SensorPoint
	for rows.Next() {
		var p ports.SensorPoint
		if err := rows.Scan(&p.Time, &p.Value, &p.Max); err != nil {
			return nil, fmt.Errorf("sensor series: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// History returns every temperature sensor's series for one node. One query,
// grouped in Go by (chip, label): the readings already share timestamps, so a
// chart can align them without the database pivoting.
func (r *SensorRepository) History(ctx context.Context, hostID uuid.UUID,
	from, to time.Time, res telemetry.Resolution) ([]ports.SensorSeries, error) {
	query := `
SELECT chip, label, crit, time, value, value
FROM host_sensors
WHERE host_id = $1 AND kind = 'temp_c' AND time >= $2 AND time <= $3
ORDER BY chip, label, time`
	if res != telemetry.ResolutionRaw {
		query = `
SELECT chip, label, crit, bucket, value_avg, value_max
FROM host_sensors_5m
WHERE host_id = $1 AND kind = 'temp_c' AND bucket >= $2 AND bucket <= $3
ORDER BY chip, label, bucket`
	}

	rows, err := r.db.Query(ctx, query, hostID, from, to)
	if err != nil {
		return nil, fmt.Errorf("sensor history: %w", err)
	}
	defer rows.Close()

	var out []ports.SensorSeries
	for rows.Next() {
		var (
			chip, label string
			crit        *float64
			p           ports.SensorPoint
		)
		if err := rows.Scan(&chip, &label, &crit, &p.Time, &p.Value, &p.Max); err != nil {
			return nil, fmt.Errorf("scan sensor history: %w", err)
		}
		// Rows are ordered by (chip, label), so a new key starts a new series.
		if len(out) == 0 || out[len(out)-1].Chip != chip || out[len(out)-1].Label != label {
			out = append(out, ports.SensorSeries{Chip: chip, Label: label, Crit: crit})
		}
		last := &out[len(out)-1]
		last.Points = append(last.Points, p)
	}
	return out, rows.Err()
}

// scanReading reads one row of the latest-readings shape.
func scanReading(rows pgx.Rows) (telemetry.Reading, time.Time, uuid.UUID, error) {
	var (
		hostID uuid.UUID
		at     time.Time
		r      telemetry.Reading
		kind   string
		value  float64
	)
	if err := rows.Scan(&hostID, &at, &r.Chip, &r.Label, &kind, &value, &r.High, &r.Crit); err != nil {
		return r, at, hostID, fmt.Errorf("scan sensor reading: %w", err)
	}
	r.Kind = telemetry.SensorKind(kind)
	r.Value = value
	return r, at, hostID, nil
}

// Get returns what is known about reaching one node.
func (r *SensorRepository) Get(ctx context.Context, hostID uuid.UUID) (ports.NodeSSH, error) {
	var rec ports.NodeSSH
	var lastTried, lastOK *time.Time
	var lastError *string
	err := r.db.QueryRow(ctx, `
SELECT host_id, address, ssh_user, algorithm, fingerprint, public_key,
       first_seen_at, last_tried_at, last_ok_at, last_error
FROM host_ssh WHERE host_id = $1`, hostID).
		Scan(&rec.HostID, &rec.Address, &rec.SSHUser, &rec.Algorithm, &rec.Fingerprint,
			&rec.PublicKey, &rec.FirstSeenAt, &lastTried, &lastOK, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.NodeSSH{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.NodeSSH{}, fmt.Errorf("get node ssh: %w", err)
	}
	if lastTried != nil {
		rec.LastTriedAt = *lastTried
	}
	if lastOK != nil {
		rec.LastOKAt = *lastOK
	}
	if lastError != nil {
		rec.LastError = *lastError
	}
	return rec, nil
}

// Pin records the key a node presented the first time it was met.
//
// ON CONFLICT DO NOTHING, deliberately: a pin is written once and only
// replaced by an operator clearing it. An upsert here would turn every
// reconnection into a re-pin and quietly defeat the mismatch check.
func (r *SensorRepository) Pin(ctx context.Context, rec ports.NodeSSH) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO host_ssh (host_id, address, ssh_user, algorithm, fingerprint, public_key, first_seen_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (host_id) DO NOTHING`,
		rec.HostID, rec.Address, rec.SSHUser, rec.Algorithm, rec.Fingerprint,
		rec.PublicKey, rec.FirstSeenAt)
	if err != nil {
		return fmt.Errorf("pin node key: %w", err)
	}
	return nil
}

// RecordAttempt saves how the last poll went.
//
// A node with no pin yet still gets a row, because "the portal tried and was
// refused" is exactly what the host page needs to show for a node whose key
// has not been installed — the state where there is nothing else to say.
func (r *SensorRepository) RecordAttempt(ctx context.Context, hostID uuid.UUID, address string,
	at time.Time, failure string) error {
	var errText *string
	if failure != "" {
		errText = &failure
	}
	var okAt *time.Time
	if failure == "" {
		okAt = &at
	}
	// The address is updated on every attempt, not only on the pin: a cluster
	// that reports a new address for a node is telling the truth, and the pin
	// is the key rather than the place it was found.
	_, err := r.db.Exec(ctx, `
INSERT INTO host_ssh (host_id, address, algorithm, fingerprint, public_key,
                      last_tried_at, last_ok_at, last_error)
VALUES ($1,$2,'','','', $3,$4,$5)
ON CONFLICT (host_id) DO UPDATE SET
    address       = EXCLUDED.address,
    last_tried_at = EXCLUDED.last_tried_at,
    last_ok_at    = COALESCE(EXCLUDED.last_ok_at, host_ssh.last_ok_at),
    last_error    = EXCLUDED.last_error`, hostID, address, at, okAt, errText)
	if err != nil {
		return fmt.Errorf("record node attempt: %w", err)
	}
	return nil
}

// Forget drops the pin so the next poll meets the node afresh.
func (r *SensorRepository) Forget(ctx context.Context, hostID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM host_ssh WHERE host_id = $1`, hostID)
	if err != nil {
		return fmt.Errorf("forget node key: %w", err)
	}
	return nil
}

// OnlineHosts names the nodes of a platform worth polling.
func (r *SensorRepository) OnlineHosts(ctx context.Context, platformID uuid.UUID) ([]ports.SensorHost, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, platform_id, external_id, name
FROM hosts
WHERE platform_id = $1 AND deleted_at IS NULL AND status = 'online'
ORDER BY name`, platformID)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var out []ports.SensorHost
	for rows.Next() {
		var h ports.SensorHost
		if err := rows.Scan(&h.ID, &h.PlatformID, &h.ExternalID, &h.Name); err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
