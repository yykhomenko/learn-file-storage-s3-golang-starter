package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"time"

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
		respondWithError(w, http.StatusUnauthorized, "You don’t own this video", nil)
		return
	}

	inputFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video inputFile", err)
		return
	}
	defer inputFile.Close()

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
		respondWithError(w, http.StatusBadRequest, "Unsupported inputFile type", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp inputFile", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, inputFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy to temp inputFile", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek to start of temp inputFile after copy", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ratio, err := getVideoAspectRatio(ctx, tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video aspect ratio", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek to start of temp inputFile after aspect ratio", err)
		return
	}

	ctx, processCancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer processCancel()

	processedFileName, err := processVideoForFastStart(ctx, tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}

	processedFile, err := os.Open(processedFileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	var prefix string
	switch ratio {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	default:
		prefix = "other/"
	}

	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't generate random name", err)
		return
	}
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)

	s3FileName := fmt.Sprintf("%s%s%s", prefix, randomString, fileExtensions[0])

	params := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3FileName,
		Body:        processedFile,
		ContentType: &mediatype,
	}

	uploadCtx, uploadCancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer uploadCancel()

	_, err = cfg.s3Client.PutObject(uploadCtx, params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3FileName)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(ctx context.Context, filePath string) (string, error) {

	cmd := exec.CommandContext(ctx,
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var output FFProbeOutput
	err = json.Unmarshal(stdout.Bytes(), &output)
	if err != nil {
		return "", err
	}

	for _, stream := range output.Streams {
		if stream.CodecType == "video" {
			switch {

			case stream.DisplayAspectRatio != "":
				switch stream.DisplayAspectRatio {
				case "16:9", "9:16":
					return stream.DisplayAspectRatio, nil
				default:
					return "other", nil
				}

			case stream.Width > 0 && stream.Height > 0:
				ratio := float64(stream.Width) / float64(stream.Height)
				if math.Abs(ratio-16.0/9.0) < 0.01 {
					return "16:9", nil
				}
				if math.Abs(ratio-9.0/16.0) < 0.01 {
					return "9:16", nil
				}
			}
		}
	}

	return "", errors.New("no video stream found")
}

func processVideoForFastStart(ctx context.Context, filePath string) (string, error) {

	processingPath := filePath + ".processing"

	cmd := exec.CommandContext(ctx,
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		processingPath)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %w, details: %s", err, stderr.String())
	}

	return processingPath, nil
}

type FFProbeOutput struct {
	Streams []struct {
		Index              int    `json:"index"`
		CodecName          string `json:"codec_name,omitempty"`
		CodecLongName      string `json:"codec_long_name,omitempty"`
		Profile            string `json:"profile,omitempty"`
		CodecType          string `json:"codec_type"`
		CodecTagString     string `json:"codec_tag_string"`
		CodecTag           string `json:"codec_tag"`
		Width              int    `json:"width,omitempty"`
		Height             int    `json:"height,omitempty"`
		CodedWidth         int    `json:"coded_width,omitempty"`
		CodedHeight        int    `json:"coded_height,omitempty"`
		HasBFrames         int    `json:"has_b_frames,omitempty"`
		SampleAspectRatio  string `json:"sample_aspect_ratio,omitempty"`
		DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
		PixFmt             string `json:"pix_fmt,omitempty"`
		Level              int    `json:"level,omitempty"`
		ColorRange         string `json:"color_range,omitempty"`
		ColorSpace         string `json:"color_space,omitempty"`
		ColorTransfer      string `json:"color_transfer,omitempty"`
		ColorPrimaries     string `json:"color_primaries,omitempty"`
		ChromaLocation     string `json:"chroma_location,omitempty"`
		FieldOrder         string `json:"field_order,omitempty"`
		Refs               int    `json:"refs,omitempty"`
		IsAvc              string `json:"is_avc,omitempty"`
		NalLengthSize      string `json:"nal_length_size,omitempty"`
		ID                 string `json:"id"`
		RFrameRate         string `json:"r_frame_rate"`
		AvgFrameRate       string `json:"avg_frame_rate"`
		TimeBase           string `json:"time_base"`
		StartPts           int    `json:"start_pts"`
		StartTime          string `json:"start_time"`
		DurationTs         int    `json:"duration_ts"`
		Duration           string `json:"duration"`
		BitRate            string `json:"bit_rate,omitempty"`
		BitsPerRawSample   string `json:"bits_per_raw_sample,omitempty"`
		NbFrames           string `json:"nb_frames"`
		ExtradataSize      int    `json:"extradata_size"`
		Disposition        struct {
			Default         int `json:"default"`
			Dub             int `json:"dub"`
			Original        int `json:"original"`
			Comment         int `json:"comment"`
			Lyrics          int `json:"lyrics"`
			Karaoke         int `json:"karaoke"`
			Forced          int `json:"forced"`
			HearingImpaired int `json:"hearing_impaired"`
			VisualImpaired  int `json:"visual_impaired"`
			CleanEffects    int `json:"clean_effects"`
			AttachedPic     int `json:"attached_pic"`
			TimedThumbnails int `json:"timed_thumbnails"`
			NonDiegetic     int `json:"non_diegetic"`
			Captions        int `json:"captions"`
			Descriptions    int `json:"descriptions"`
			Metadata        int `json:"metadata"`
			Dependent       int `json:"dependent"`
			StillImage      int `json:"still_image"`
			Multilayer      int `json:"multilayer"`
		} `json:"disposition"`
		Tags struct {
			Language    string `json:"language"`
			HandlerName string `json:"handler_name"`
			VendorID    string `json:"vendor_id"`
			Encoder     string `json:"encoder"`
			Timecode    string `json:"timecode"`
		} `json:"tags,omitempty"`
		SampleFmt      string `json:"sample_fmt,omitempty"`
		SampleRate     string `json:"sample_rate,omitempty"`
		Channels       int    `json:"channels,omitempty"`
		ChannelLayout  string `json:"channel_layout,omitempty"`
		BitsPerSample  int    `json:"bits_per_sample,omitempty"`
		InitialPadding int    `json:"initial_padding,omitempty"`
	} `json:"streams"`
}
