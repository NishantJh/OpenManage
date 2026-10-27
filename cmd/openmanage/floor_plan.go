package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The floor plan is a single drawn layout (metres): rooms and where each access point
// sits. It is data for the UI only and never changes device configuration.
const (
	floorPlanBodyLimit = 64 * 1024
	floorPlanMaxRooms  = 60
	floorPlanMaxExtent = 500.0
)

var roomIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

type floorRoom struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

type floorPlacement struct {
	DeviceID string  `json:"device_id"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
}

type floorLayout struct {
	Rooms      []floorRoom      `json:"rooms"`
	Placements []floorPlacement `json:"placements"`
	Image      *floorImage      `json:"image,omitempty"`
}

// overlaps reports whether two rooms share floor area; rooms that only touch are fine.
func (r floorRoom) overlaps(o floorRoom) bool {
	const eps = 0.01
	return r.X < o.X+o.W-eps && r.X+r.W > o.X+eps && r.Y < o.Y+o.H-eps && r.Y+r.H > o.Y+eps
}

func inExtent(v float64) bool { return v >= 0 && v <= floorPlanMaxExtent }

func (l *floorLayout) validate() error {
	if l.Rooms == nil {
		l.Rooms = []floorRoom{}
	}
	if l.Placements == nil {
		l.Placements = []floorPlacement{}
	}
	if l.Image != nil {
		if err := l.Image.validate(); err != nil {
			return err
		}
	}
	if len(l.Rooms) > floorPlanMaxRooms {
		return fmt.Errorf("at most %d rooms", floorPlanMaxRooms)
	}
	ids := map[string]bool{}
	for i := range l.Rooms {
		r := &l.Rooms[i]
		r.Name = strings.TrimSpace(r.Name)
		if !roomIDPattern.MatchString(r.ID) || ids[r.ID] {
			return errors.New("room IDs must be unique short identifiers")
		}
		ids[r.ID] = true
		if r.Name == "" || len(r.Name) > 60 {
			return errors.New("room names must be 1-60 characters")
		}
		if !inExtent(r.X) || !inExtent(r.Y) || r.W < 0.5 || r.H < 0.5 || r.W > floorPlanMaxExtent || r.H > floorPlanMaxExtent {
			return errors.New("room position or size is out of range")
		}
	}
	for i := range l.Rooms {
		for j := i + 1; j < len(l.Rooms); j++ {
			if l.Rooms[i].overlaps(l.Rooms[j]) {
				return errors.New("rooms must not overlap")
			}
		}
	}
	if len(l.Placements) > 200 {
		return errors.New("too many placements")
	}
	seen := map[string]bool{}
	for _, p := range l.Placements {
		if !validUUID(p.DeviceID) || seen[p.DeviceID] {
			return errors.New("placements need a unique device ID")
		}
		seen[p.DeviceID] = true
		if !inExtent(p.X) || !inExtent(p.Y) {
			return errors.New("placement is out of range")
		}
	}
	return nil
}

type floorPlanResponse struct {
	floorLayout
	UpdatedAt time.Time `json:"updated_at"`
}

func (a *app) getFloorPlan(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	var resp floorPlanResponse
	if err := a.db.QueryRow(r.Context(), `SELECT layout, updated_at FROM floor_plan WHERE id=TRUE`).Scan(&raw, &resp.UpdatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load floor plan")
		return
	}
	if err := json.Unmarshal(raw, &resp.floorLayout); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load floor plan")
		return
	}
	_ = resp.floorLayout.validate()
	if err := fillImage(r.Context(), a.db, &resp.floorLayout); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load floor plan")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) putFloorPlan(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	var in floorLayout
	if err := decodeJSONBody(w, r, floorPlanBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid floor plan")
		return
	}
	if err := in.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save floor plan")
		return
	}
	defer tx.Rollback(r.Context())
	// The picture itself is uploaded separately; a layout can only position one that is stored,
	// and a client that does not mention the image keeps the placement it already had.
	stored, err := lockLayout(r.Context(), tx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save floor plan")
		return
	}
	if in.Image == nil {
		in.Image = stored.Image
	} else {
		g := in.Image.geometry()
		in.Image = &g
	}
	if in.Image != nil {
		if err := fillImage(r.Context(), tx, &in); err != nil {
			writeError(w, http.StatusInternalServerError, "could not save floor plan")
			return
		}
	}
	if in.Image != nil && in.Image.heightM() > floorPlanMaxExtent {
		writeError(w, http.StatusBadRequest, "the plan image is too tall for its width")
		return
	}
	if in.Image != nil {
		g := in.Image.geometry()
		in.Image = &g
	}
	raw, _ := json.Marshal(in)
	var resp floorPlanResponse
	resp.floorLayout = in
	// Placements must point at existing devices; the foreign-key-like check keeps stale IDs out.
	for _, p := range in.Placements {
		var one int
		if err := tx.QueryRow(r.Context(), `SELECT 1 FROM devices WHERE id=$1`, p.DeviceID).Scan(&one); err != nil {
			writeError(w, http.StatusBadRequest, "placement refers to an unknown device")
			return
		}
	}
	err = tx.QueryRow(r.Context(), `UPDATE floor_plan SET layout=$1, updated_by=$2, updated_at=NOW() WHERE id=TRUE RETURNING updated_at`,
		raw, actor.ID).Scan(&resp.UpdatedAt)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "floor_plan.update", "floor_plan", "",
			map[string]any{"rooms": len(in.Rooms), "placements": len(in.Placements)})
	}
	if err == nil {
		err = fillImage(r.Context(), tx, &resp.floorLayout)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save floor plan")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
