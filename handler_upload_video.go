package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.ParseMultipartForm(maxMemory)

	//Copied from the image uploader
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
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
	}

	videoFile, fileHeader, err := r.FormFile("video")

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request", err)
		return
	}
	defer videoFile.Close()

	contentType := fileHeader.Header.Get("Content-Type")

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid video format", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temporary file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, videoFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying temporary file", err)
		return
	}

	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error resetting temporary file", err)
		return
	}

	key := make([]byte, 32)
	rand.Read(key)
	filekey := base64.RawURLEncoding.EncodeToString(key) + ".mp4"

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading video attributes", err)
		return
	}
	if aspectRatio == "16:9" {
		filekey = "landscape/" + filekey
	} else if aspectRatio == "9:16" {
		filekey = "portrait/" + filekey
	} else {
		filekey = "other/" + filekey
	}

	fastPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video for fast start", err)
		return
	}
	fastFile, err := os.Open(fastPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open file for fast start", err)
		return
	}
	defer os.Remove(fastPath)
	defer fastFile.Close()

	params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &filekey,
		Body:        fastFile,
		ContentType: &mediaType,
	}

	_, err = cfg.s3Client.PutObject(
		r.Context(),
		&params,
	)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, filekey)
	log.Println(videoURL)
	video.VideoURL = &videoURL

	if err = cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}

func checkRatio(a, b, c, d int) bool {
	const accuracy = 0.001
	diff := math.Abs(float64(a)/float64(b) - float64(c)/float64(d))
	return diff <= accuracy
}

func getVideoAspectRatio(filePath string) (string, error) {
	type videoData struct {
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
			ClosedCaptions     int    `json:"closed_captions,omitempty"`
			FilmGrain          int    `json:"film_grain,omitempty"`
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
			Tags0          struct {
				Language    string `json:"language"`
				HandlerName string `json:"handler_name"`
				VendorID    string `json:"vendor_id"`
			} `json:"tags,omitempty"`
			Tags1 struct {
				Language    string `json:"language"`
				HandlerName string `json:"handler_name"`
				Timecode    string `json:"timecode"`
			} `json:"tags,omitempty"`
		} `json:"streams"`
	}

	getVidInfo := exec.Command("ffprobe",
		"-v", "error", "-print_format", "json", "-show_streams", filePath)

	var buff bytes.Buffer
	getVidInfo.Stdout = &buff

	err := getVidInfo.Run()
	if err != nil {
		return "", err
	}

	var data videoData
	err = json.Unmarshal(buff.Bytes(), &data)
	if err != nil {
		return "", err
	}
	width := data.Streams[0].Width
	height := data.Streams[0].Height

	if checkRatio(width, height, 16, 9) {
		return "16:9", nil
	} else if checkRatio(width, height, 9, 16) {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"

	fastStart := exec.Command("ffmpeg",
		"-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	err := fastStart.Run()
	if err != nil {
		return "", err
	}
	return outputPath, nil
}
