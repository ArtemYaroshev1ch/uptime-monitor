package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/middleware"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/response"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/service"
)

// ChannelService is the part of service.Channels the handlers use.
type ChannelService interface {
	Create(ctx context.Context, userID uuid.UUID, p service.ChannelPatch) (domain.Channel, error)
	Get(ctx context.Context, userID, id uuid.UUID) (domain.Channel, error)
	List(ctx context.Context, userID uuid.UUID, limit, offset int) (service.ChannelPage, error)
	Update(ctx context.Context, userID, id uuid.UUID, p service.ChannelPatch) (domain.Channel, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	Test(ctx context.Context, userID, id uuid.UUID) (service.TestResult, error)
}

// Channels serves /api/v1/channels for the authenticated user.
type Channels struct {
	svc ChannelService
	log *slog.Logger
}

// NewChannels returns the channel handlers.
func NewChannels(svc ChannelService, log *slog.Logger) Channels {
	return Channels{svc: svc, log: log}
}

// channelConfigResponse is the config as the API shows it. The webhook
// secret is set only in the response to create.
type channelConfigResponse struct {
	ChatID string `json:"chat_id,omitempty"`
	URL    string `json:"url,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type channelResponse struct {
	ID        uuid.UUID             `json:"id"`
	Type      string                `json:"type"`
	Name      string                `json:"name"`
	Config    channelConfigResponse `json:"config"`
	IsActive  bool                  `json:"is_active"`
	CreatedAt time.Time             `json:"created_at"`
}

// newChannelResponse renders a channel without its secret. The response
// type has the field, but only Create fills it in.
func newChannelResponse(ch domain.Channel) channelResponse {
	return channelResponse{
		ID:        ch.ID,
		Type:      string(ch.Type),
		Name:      ch.Name,
		Config:    channelConfigResponse{ChatID: ch.Config.ChatID, URL: ch.Config.URL},
		IsActive:  ch.IsActive,
		CreatedAt: ch.CreatedAt.UTC(),
	}
}

type testResponse struct {
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

// Create handles POST /api/v1/channels. For a webhook the response is the
// only place the signing secret is ever shown.
func (h Channels) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.user(w, r)
	if !ok {
		return
	}
	var p service.ChannelPatch
	if err := decodeJSON(r, &p); err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	ch, err := h.svc.Create(r.Context(), userID, p)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	h.log.InfoContext(r.Context(), "channel created",
		slog.String("channel_id", ch.ID.String()), slog.String("channel_type", string(ch.Type)))
	res := newChannelResponse(ch)
	res.Config.Secret = ch.Config.Secret
	w.Header().Set("Location", "/api/v1/channels/"+ch.ID.String())
	// The secret must not end up in a shared cache.
	w.Header().Set("Cache-Control", "no-store")
	response.JSON(w, http.StatusCreated, res)
}

// List handles GET /api/v1/channels.
func (h Channels) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.user(w, r)
	if !ok {
		return
	}
	pg, err := parsePage(r)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	res, err := h.svc.List(r.Context(), userID, pg.Limit, pg.Offset)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	items := make([]channelResponse, 0, len(res.Items))
	for _, ch := range res.Items {
		items = append(items, newChannelResponse(ch))
	}
	response.JSON(w, http.StatusOK, listResponse[channelResponse]{Items: items, Total: res.Total})
}

// Get handles GET /api/v1/channels/{id}.
func (h Channels) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, r, ok := h.userAndChannel(w, r)
	if !ok {
		return
	}
	ch, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	response.JSON(w, http.StatusOK, newChannelResponse(ch))
}

// Update handles PATCH /api/v1/channels/{id}.
func (h Channels) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, r, ok := h.userAndChannel(w, r)
	if !ok {
		return
	}
	var p service.ChannelPatch
	if err := decodeJSON(r, &p); err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	ch, err := h.svc.Update(r.Context(), userID, id, p)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	response.JSON(w, http.StatusOK, newChannelResponse(ch))
}

// Delete handles DELETE /api/v1/channels/{id}.
func (h Channels) Delete(w http.ResponseWriter, r *http.Request) {
	userID, id, r, ok := h.userAndChannel(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), userID, id); err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	h.log.InfoContext(r.Context(), "channel deleted")
	w.WriteHeader(http.StatusNoContent)
}

// Test handles POST /api/v1/channels/{id}/test. The delivery outcome is in
// the body: 200 means the test was carried out, not that it succeeded.
func (h Channels) Test(w http.ResponseWriter, r *http.Request) {
	userID, id, r, ok := h.userAndChannel(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Test(r.Context(), userID, id)
	if err != nil {
		response.Error(w, r, h.log, err)
		return
	}
	if res.Delivered {
		h.log.InfoContext(r.Context(), "test notification delivered")
	} else {
		h.log.WarnContext(r.Context(), "test notification failed", slog.String("error", res.Error))
	}
	response.JSON(w, http.StatusOK, testResponse(res))
}

func (h Channels) user(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := middleware.UserID(r.Context())
	if !ok {
		response.Error(w, r, h.log, fmt.Errorf("channels: route is not authenticated"))
	}
	return userID, ok
}

// userAndChannel also parses {id}; a malformed ID is a 404 like an unknown
// or foreign one. The returned request's context logs channel_id.
func (h Channels) userAndChannel(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, *http.Request, bool) {
	userID, ok := h.user(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, r, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, r, h.log, fmt.Errorf("channel id: %w", domain.ErrNotFound))
		return uuid.Nil, uuid.Nil, r, false
	}
	ctx := logger.WithAttrs(r.Context(), slog.String("channel_id", id.String()))
	return userID, id, r.WithContext(ctx), true
}
