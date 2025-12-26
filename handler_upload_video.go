package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	const maxFileSize = 10 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)

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

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file", err)
		return
	}
	defer file.Close()

	mediatype, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}

	if mediatype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type", err)
		return
	}

	fileExtensions, err := mime.ExtensionsByType(mediatype)
	if err != nil || len(fileExtensions) == 0 {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy to temp file", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek to start of temp file", err)
		return
	}

	randomBytes := make([]byte, 32)
	n, err := rand.Read(randomBytes)
	if err != nil || n != 32 {
		respondWithError(w, http.StatusBadRequest, "Couldn't generate random name", err)
		return
	}

	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)
	randomFileName := fmt.Sprintf("%s%s", randomString, fileExtensions[0])

	params := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &randomFileName,
		Body:        tempFile,
		ContentType: &mediatype,
	}

	_, err = cfg.s3Client.PutObject(context.Background(), params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, randomFileName)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
