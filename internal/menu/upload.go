package menu

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/response"
)

const maxUploadSize = 5 << 20 // 5MB

var allowedImageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// UploadHandler handles image uploads for menu items.
type UploadHandler struct {
	storage *configsvc.Storage
}

// NewUploadHandler creates a new upload handler with the given storage facade.
func NewUploadHandler(storage *configsvc.Storage) *UploadHandler {
	return &UploadHandler{storage: storage}
}

// UploadRoutes registers the upload endpoint on the given router.
func (h *UploadHandler) UploadRoutes(r chi.Router) {
	r.Post("/upload", h.handleUpload)
}

func (h *UploadHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "File too large (max 5MB)")
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "No image file provided")
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	ext, ok := allowedImageTypes[contentType]
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Invalid file type. Use JPEG, PNG, or WebP")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "Failed to read file")
		return
	}

	if !isValidImage(data, contentType) {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Invalid image file")
		return
	}

	tid := tenantID(r)
	key := fmt.Sprintf("menu-items/%s/%s%s", tid, uuid.New().String(), ext)

	obj, err := h.storage.Put(r.Context(), tid, configsvc.PutRequest{
		Key:         key,
		Body:        data,
		ContentType: contentType,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "Failed to upload image")
		return
	}

	url := obj.URL
	if url == "" {
		url, err = h.storage.PresignGet(r.Context(), tid, obj.Key, 15*time.Minute)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "Failed to generate URL")
			return
		}
	}

	response.JSON(w, http.StatusOK, map[string]string{"url": url})
}

func isValidImage(data []byte, contentType string) bool {
	switch contentType {
	case "image/jpeg":
		return len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8
	case "image/png":
		return len(data) > 8 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47
	case "image/webp":
		return len(data) > 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	}
	return false
}
