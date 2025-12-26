package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}

	fileIn, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get file", err)
		return
	}
	defer fileIn.Close()

	mediatype, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}

	if mediatype != "image/jpeg" && mediatype != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type", err)
		return
	}

	fileExtensions, err := mime.ExtensionsByType(mediatype)
	if err != nil || len(fileExtensions) == 0 {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", err)
		return
	}

	randomBytes := make([]byte, 32)
	n, err := rand.Read(randomBytes)
	if err != nil || n != 32 {
		respondWithError(w, http.StatusBadRequest, "Couldn't generate random name", err)
		return
	}

	randomName := base64.RawURLEncoding.EncodeToString(randomBytes)

	fileName := fmt.Sprintf("%s%s", randomName, fileExtensions[0])
	filePath := filepath.Join(cfg.assetsRoot, fileName)

	fileOut, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file", err)
		return
	}
	defer fileOut.Close()

	_, err = io.Copy(fileOut, fileIn)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write file", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	case err != nil:
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	case video.UserID != userID:
		respondWithError(w, http.StatusUnauthorized, "Couldn't get video", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, fileName)
	video.ThumbnailURL = &thumbnailURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
