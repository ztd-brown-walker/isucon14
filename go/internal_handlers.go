package main

import (
	"context"
	"database/sql"
	"math"
	"net/http"
)

// 距離 1 を進むのにかかる実時間（秒）。2026-10-25 19:49 のベンチで、乗せてから目的地に着くまでの
// 時間を乗車距離で割った実測値。速さ 3 は名目上 2 の 1.5 倍だが、実時間は 2 と変わらなかった
var secondsPerDistance = map[int]float64{2: 0.066, 3: 0.066, 5: 0.045, 7: 0.032}

// 実測のない速さは、速さ 2 の実測値から速さに反比例するとみなす
func travelSecondsPerDistance(speed int) float64 {
	if s, ok := secondsPerDistance[speed]; ok {
		return s
	}
	return 0.132 / float64(speed)
}

// 位置をまだ送っていない椅子のコスト。ほかに空きがないときだけ使われるよう、どの組よりも大きくする
const unknownLocationCost = 1e9

type matchingChair struct {
	ID        string        `db:"id"`
	Speed     int           `db:"speed"`
	Latitude  sql.NullInt64 `db:"latitude"`
	Longitude sql.NullInt64 `db:"longitude"`
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
// 待っているライド × 空いている椅子について「迎車 + 移動」の実時間を見積もり、
// 全体の合計が最小になる組み合わせをハンガリアン法で決める
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

	// 椅子が足りないときは、待たせているライド（rides は created_at 順）から割り当てる
	if len(rides) > len(chairs) {
		rides = rides[:len(chairs)]
	}

	costs := make([][]float64, len(rides))
	for i, ride := range rides {
		trip := calculateDistance(ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
		costs[i] = make([]float64, len(chairs))
		for j, chair := range chairs {
			if !chair.Latitude.Valid || !chair.Longitude.Valid {
				costs[i][j] = unknownLocationCost
				continue
			}
			pickup := calculateDistance(int(chair.Latitude.Int64), int(chair.Longitude.Int64), ride.PickupLatitude, ride.PickupLongitude)
			costs[i][j] = float64(pickup+trip) * travelSecondsPerDistance(chair.Speed)
		}
	}

	for i, j := range assignMinCost(costs) {
		ride := rides[i]
		chairID := chairs[j].ID
		if err := assignRide(ctx, ride.ID, ride.UserID, chairID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// assignMinCost は行（ライド）ごとに列（椅子）を 1 つずつ、コストの合計が最小になるように割り当てる（ハンガリアン法）。
// 行数は列数以下であること。戻り値の i 番目が行 i に割り当てた列。計算量は O(行数² × 列数)
func assignMinCost(costs [][]float64) []int {
	n := len(costs)
	if n == 0 {
		return nil
	}
	m := len(costs[0])
	// 1 始まりで持ち、0 番目を番兵に使う
	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1) // p[j]: 列 j に割り当てた行
	way := make([]int, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, m+1)
		used := make([]bool, m+1)
		for j := range minv {
			minv[j] = math.Inf(1)
		}
		for {
			used[j0] = true
			i0, delta, j1 := p[j0], math.Inf(1), 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				if cur := costs[i0-1][j-1] - u[i0] - v[j]; cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}
	assigned := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			assigned[p[j]-1] = j - 1
		}
	}
	return assigned
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
