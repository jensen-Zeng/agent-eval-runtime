package api

import "net/http"

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) { h.submit(w, r) }
