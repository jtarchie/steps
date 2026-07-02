package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Handler serves the user API.
type Handler struct {
	store Store
}

// Store abstracts user lookups.
type Store interface {
	Find(ctx interface{ Done() <-chan struct{} }, id string) (*User, error)
}

// User is the persisted account record.
type User struct {
	ID    string
	Name  string
	Email string
}

// Profile is the public projection of a user.
func (u *User) Profile() map[string]string {
	return map[string]string{"id": u.ID, "name": u.Name}
}

// GetUser returns a user's public profile.
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u, _ := h.store.Find(r.Context(), id)
	if u == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, u.Profile())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
