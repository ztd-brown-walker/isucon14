package main

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"sort"
)

type matchingChair struct {
	ID        string        `db:"id"`
	Speed     int           `db:"speed"`
	Latitude  sql.NullInt64 `db:"latitude"`
	Longitude sql.NullInt64 `db:"longitude"`
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
// 待っているライド × 空いている椅子を総当たりし、迎車にかかる時間（距離 ÷ 速さ）が短い組から順に確定する
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 空いている椅子 = 割り当て済みのライドが、6 つの状態すべてを椅子に通知し終えている椅子
	chairs := []matchingChair{}
	if err := db.SelectContext(ctx, &chairs, `
		SELECT c.id, m.speed, l.latitude, l.longitude
		FROM chairs c
		JOIN chair_models m ON m.name = c.model
		LEFT JOIN chair_locations l ON l.id = (
			SELECT id FROM chair_locations WHERE chair_id = c.id ORDER BY created_at DESC LIMIT 1
		)
		WHERE c.is_active = TRUE
		  AND NOT EXISTS (
			SELECT 1 FROM rides r
			WHERE r.chair_id = c.id
			  AND (SELECT COUNT(rs.chair_sent_at) FROM ride_statuses rs WHERE rs.ride_id = r.id) < 6
		  )`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	type pair struct {
		ride  int
		chair int
		cost  float64
	}
	pairs := make([]pair, 0, len(rides)*len(chairs))
	for i, ride := range rides {
		for j, chair := range chairs {
			// 位置をまだ送っていない椅子は、ほかに空きがないときだけ使う
			cost := math.Inf(1)
			if chair.Latitude.Valid && chair.Longitude.Valid {
				d := calculateDistance(int(chair.Latitude.Int64), int(chair.Longitude.Int64), ride.PickupLatitude, ride.PickupLongitude)
				cost = float64(d) / float64(chair.Speed)
			}
			pairs = append(pairs, pair{ride: i, chair: j, cost: cost})
		}
	}
	// コストが同じなら、待たせているライド（rides は created_at 順）を優先する
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].cost < pairs[b].cost })

	rideUsed := make([]bool, len(rides))
	chairUsed := make([]bool, len(chairs))
	for _, p := range pairs {
		if rideUsed[p.ride] || chairUsed[p.chair] {
			continue
		}
		rideUsed[p.ride] = true
		chairUsed[p.chair] = true

		ride := rides[p.ride]
		chairID := chairs[p.chair].ID
		if err := assignRide(ctx, ride.ID, ride.UserID, chairID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// assignRide changes the assignment and display availability atomically.
// Candidate selection above still waits for all six chair notifications.
func assignRide(ctx context.Context, rideID, userID, chairID string) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", chairID, rideID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE chairs SET incomplete_ride_count = incomplete_ride_count + 1, updated_at = updated_at WHERE id = ?", chairID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	notifyRide(userID, chairID)
	return nil
}
