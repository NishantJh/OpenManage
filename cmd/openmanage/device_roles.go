package main

import (
	"net/http"
)

func validDeviceRole(role string) bool { return role == "access_point" || role == "router" }

// updateDeviceRole sets what an enrolled device is for. It changes only how OpenManage
// reads names for Wi-Fi clients; nothing is written to the device itself.
func (a *app) updateDeviceRole(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	var in struct {
		Role     *string `json:"role"`
		DHCPHost *bool   `json:"dhcp_host"`
	}
	if err := decodeJSONBody(w, r, deviceBodyLimit, &in); err != nil || (in.Role == nil && in.DHCPHost == nil) {
		writeError(w, http.StatusBadRequest, "request must set role or dhcp_host")
		return
	}
	if in.Role != nil && !validDeviceRole(*in.Role) {
		writeError(w, http.StatusBadRequest, "role must be access_point or router")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update device")
		return
	}
	defer tx.Rollback(r.Context())
	var d device
	err = tx.QueryRow(r.Context(), `UPDATE devices SET role=COALESCE($2,role), dhcp_host=COALESCE($3,dhcp_host) WHERE id=$1
		RETURNING `+deviceColumns, id, in.Role, in.DHCPHost).
		Scan(&d.ID, &d.Name, &d.Address, &d.Model, &d.Firmware, &d.Status, &d.LastSeenAt, &d.CreatedAt, &d.ConfigDrift, &d.Role, &d.DHCPHost, &d.HasKey)
	if err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "device.update_role", "device", d.ID,
		map[string]any{"role": d.Role, "dhcp_host": d.DHCPHost}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update device and record audit event")
		return
	}
	d.Enrollable = isPrivateDeviceIP(d.Address)
	writeJSON(w, http.StatusOK, d)
}
