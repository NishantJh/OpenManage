package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// The plan image is a PNG or JPEG the user uploads to trace rooms over. It is stored as bytes,
// never interpreted beyond its header, and only served back with its sniffed type (no SVG).
const (
	floorImageMaxBytes  = 5 << 20
	floorImageMaxSide   = 8000
	floorImageMaxPixels = 40_000_000
	floorImageDefaultM  = 12.0
)

// floorImage is where the image sits on the plan (metres): its top-left corner and its width;
// the height follows from the picture's own aspect ratio. PxW/PxH/Version are filled on read.
type floorImage struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	WidthM  float64 `json:"width_m"`
	PxW     int     `json:"px_w,omitempty"`
	PxH     int     `json:"px_h,omitempty"`
	Version string  `json:"version,omitempty"`
}

func (i *floorImage) validate() error {
	if !inExtent(i.X) || !inExtent(i.Y) || i.WidthM < 1 || i.WidthM > floorPlanMaxExtent {
		return errors.New("plan image position or size is out of range")
	}
	return nil
}

// geometry is what is kept in the layout document; the picture's own facts come from its row.
func (i floorImage) geometry() floorImage { return floorImage{X: i.X, Y: i.Y, WidthM: i.WidthM} }

// fillImage adds the stored picture's facts to a layout. A layout that names an image that is
// no longer stored simply loses it.
func fillImage(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, l *floorLayout) error {
	if l.Image == nil {
		return nil
	}
	var w, h int
	var at time.Time
	err := q.QueryRow(ctx, `SELECT width_px, height_px, updated_at FROM floor_plan_image WHERE id=TRUE`).Scan(&w, &h, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		l.Image = nil
		return nil
	}
	if err != nil {
		return err
	}
	l.Image.PxW, l.Image.PxH, l.Image.Version = w, h, strconv.FormatInt(at.UnixNano(), 10)
	return nil
}

// sniffImage checks the bytes really are a PNG or JPEG of a sane size and returns its type
// and dimensions. It reads only the header, so a decompression bomb is never expanded.
func sniffImage(data []byte) (contentType string, w, h int, err error) {
	switch http.DetectContentType(data) {
	case "image/png":
		contentType = "image/png"
	case "image/jpeg":
		contentType = "image/jpeg"
	default:
		return "", 0, 0, errors.New("the plan image must be a PNG or JPEG")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, errors.New("the plan image could not be read")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > floorImageMaxSide || cfg.Height > floorImageMaxSide || cfg.Width*cfg.Height > floorImageMaxPixels {
		return "", 0, 0, errors.New("the plan image is too large (at most 8000 px a side)")
	}
	return contentType, cfg.Width, cfg.Height, nil
}

func (a *app) getFloorPlanImage(w http.ResponseWriter, r *http.Request) {
	var ct string
	var data []byte
	var at time.Time
	err := a.db.QueryRow(r.Context(), `SELECT content_type, data, updated_at FROM floor_plan_image WHERE id=TRUE`).Scan(&ct, &data, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no plan image")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load plan image")
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// putFloorPlanImage stores the uploaded picture. The layout keeps its rooms; if it had no image
// placement yet the picture starts at the origin with a default width the user then scales.
func (a *app) putFloorPlanImage(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, floorImageMaxBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the plan image is too large (at most 5 MB)")
		return
	}
	ct, pxW, pxH, err := sniffImage(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save plan image")
		return
	}
	defer tx.Rollback(r.Context())
	layout, err := lockLayout(r.Context(), tx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save plan image")
		return
	}
	if layout.Image == nil {
		layout.Image = &floorImage{WidthM: floorImageDefaultM}
	}
	raw, _ := json.Marshal(layout)
	var resp floorPlanResponse
	resp.floorLayout = layout
	if _, err = tx.Exec(r.Context(), `INSERT INTO floor_plan_image (id, content_type, data, width_px, height_px, updated_by, updated_at)
		VALUES (TRUE, $1, $2, $3, $4, $5, NOW())
		ON CONFLICT (id) DO UPDATE SET content_type=$1, data=$2, width_px=$3, height_px=$4, updated_by=$5, updated_at=NOW()`,
		ct, data, pxW, pxH, actor.ID); err == nil {
		err = tx.QueryRow(r.Context(), `UPDATE floor_plan SET layout=$1, updated_by=$2, updated_at=NOW() WHERE id=TRUE RETURNING updated_at`,
			raw, actor.ID).Scan(&resp.UpdatedAt)
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "floor_plan.image_upload", "floor_plan", "", map[string]any{"bytes": len(data), "type": ct})
	}
	if err == nil {
		err = fillImage(r.Context(), tx, &resp.floorLayout)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save plan image")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) deleteFloorPlanImage(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove plan image")
		return
	}
	defer tx.Rollback(r.Context())
	layout, err := lockLayout(r.Context(), tx)
	if err == nil {
		layout.Image = nil
		raw, _ := json.Marshal(layout)
		if _, err = tx.Exec(r.Context(), `DELETE FROM floor_plan_image WHERE id=TRUE`); err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE floor_plan SET layout=$1, updated_by=$2, updated_at=NOW() WHERE id=TRUE`, raw, actor.ID)
		}
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "floor_plan.image_delete", "floor_plan", "", nil)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove plan image")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lockLayout reads the stored layout inside a transaction and locks the row so concurrent
// image and layout saves cannot overwrite each other.
func lockLayout(ctx context.Context, tx pgx.Tx) (floorLayout, error) {
	var raw []byte
	var l floorLayout
	if err := tx.QueryRow(ctx, `SELECT layout FROM floor_plan WHERE id=TRUE FOR UPDATE`).Scan(&raw); err != nil {
		return l, err
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return l, err
	}
	if l.Image != nil {
		g := l.Image.geometry()
		l.Image = &g
	}
	return l, nil
}
