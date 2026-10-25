package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.ExecContext(ctx, "UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	trace := newCoordinateTrace(w, r)
	w = trace.response
	defer trace.finish()
	trace.stage("decode_request")
	ctx := r.Context()
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := ctx.Value("chair").(*Chair)
	trace.fields["chair_id"] = chair.ID

	trace.stage("begin_tx")
	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	trace.stage("insert_location")
	chairLocationID := ulid.Make().String()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO chair_locations (id, chair_id, latitude, longitude) VALUES (?, ?, ?, ?)`,
		chairLocationID, chair.ID, req.Latitude, req.Longitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	trace.stage("get_location")
	location := &ChairLocation{}
	if err := tx.GetContext(ctx, location, `SELECT * FROM chair_locations WHERE id = ?`, chairLocationID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	trace.stage("get_latest_ride")
	ride := &Ride{}
	statusChanged := false
	if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		trace.fields["ride_id"] = ride.ID
		trace.stage("get_latest_status")
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		trace.fields["ride_status"] = status
		trace.stage("check_coordinates")
		if status != "COMPLETED" && status != "CANCELED" {
			if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude && status == "ENROUTE" {
				trace.stage("insert_pickup_status")
				if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "PICKUP"); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				statusChanged = true
			}

			if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude && status == "CARRYING" {
				trace.stage("insert_arrived_status")
				if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "ARRIVED"); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				statusChanged = true
			}
		}
	}

	trace.stage("commit")
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if statusChanged {
		trace.stage("notify_ride")
		notifyRide(ride.UserID, chair.ID)
	}

	trace.fields["status_changed"] = statusChanged
	trace.stage("write_response")
	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: location.CreatedAt.UnixMilli(),
	})
}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

func chairGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	wake, unsubscribe := chairNotifier.subscribe(chair.ID)
	defer unsubscribe()

	serveNotificationSSE(ctx, w, wake, func(force bool) (bool, error) {
		return sendChairNotification(ctx, w, chair, force)
	})
}

// 椅子の最新のライドに未通知の状態があれば、古いものから1件送る。
// force のときは、未通知の状態がなくても最新の状態を送る。送ったかどうかを返す
func sendChairNotification(ctx context.Context, w http.ResponseWriter, chair *Chair, force bool) (bool, error) {
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	yetSentRideStatus := RideStatus{}
	status := ""
	if err := db.GetContext(ctx, &yetSentRideStatus, `SELECT * FROM ride_statuses WHERE ride_id = ? AND chair_sent_at IS NULL ORDER BY created_at ASC LIMIT 1`, ride.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if !force {
			return false, nil
		}
		status, err = getLatestRideStatus(ctx, db, ride.ID)
		if err != nil {
			return false, err
		}
	} else {
		status = yetSentRideStatus.Status
	}

	user := &User{}
	if err := db.GetContext(ctx, user, "SELECT * FROM users WHERE id = ?", ride.UserID); err != nil {
		return false, err
	}

	if yetSentRideStatus.ID != "" {
		if _, err := db.ExecContext(ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, yetSentRideStatus.ID); err != nil {
			return false, err
		}
	}

	if err := writeSSE(w, &chairGetNotificationResponseData{
		RideID: ride.ID,
		User: simpleUser{
			ID:   user.ID,
			Name: fmt.Sprintf("%s %s", user.Firstname, user.Lastname),
		},
		PickupCoordinate: Coordinate{
			Latitude:  ride.PickupLatitude,
			Longitude: ride.PickupLongitude,
		},
		DestinationCoordinate: Coordinate{
			Latitude:  ride.DestinationLatitude,
			Longitude: ride.DestinationLongitude,
		},
		Status: status,
	}); err != nil {
		return false, err
	}
	return true, nil
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rideID := r.PathValue("ride_id")

	chair := ctx.Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT * FROM rides WHERE id = ? FOR UPDATE", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "ENROUTE"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	// After Picking up user
	case "CARRYING":
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "CARRYING"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	notifyRide(ride.UserID, chair.ID)

	w.WriteHeader(http.StatusNoContent)
}
