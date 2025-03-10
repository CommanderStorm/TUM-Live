package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joschahenningsen/TUM-Live/worker/cfg"
	"github.com/joschahenningsen/TUM-Live/worker/pb"
	log "github.com/sirupsen/logrus"
)

type safeStreams struct {
	mutex   sync.Mutex
	streams map[uint32][]*StreamContext // Note that we can have multiple contexts for a streamID for different sources.
}

// regularStreams keeps track of all lecture hall streams for the current worker
var regularStreams = safeStreams{streams: make(map[uint32][]*StreamContext)}

// addContext adds a stream context for a given streamID to the map in safeStreams
func (s *safeStreams) addContext(streamID uint32, streamCtx *StreamContext) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.streams[streamID] = append(s.streams[streamCtx.streamId], streamCtx)
}

func HandlePremiere(request *pb.PremiereRequest) {
	streamCtx := &StreamContext{
		streamId:      request.StreamID,
		sourceUrl:     request.FilePath,
		startTime:     time.Now(),
		streamVersion: "",
		courseSlug:    "PREMIERE",
		ingestServer:  request.IngestServer,
		outUrl:        request.OutUrl,
	}
	// Register worker for premiere
	if !streamCtx.isSelfStream {
		regularStreams.addContext(streamCtx.streamId, streamCtx)
	}
	S.startStream(streamCtx)
	streamPremiere(streamCtx)
	S.endStream(streamCtx)
	NotifyStreamDone(streamCtx)
}

func HandleSelfStream(request *pb.SelfStreamResponse, slug string) *StreamContext {
	streamCtx := &StreamContext{
		streamId:      request.GetStreamID(),
		courseSlug:    request.GetCourseSlug(),
		teachingTerm:  request.GetCourseTerm(),
		teachingYear:  request.GetCourseYear(),
		startTime:     request.GetStreamStart().AsTime().Local(),
		endTime:       time.Now().Add(time.Hour * 7),
		publishVoD:    request.GetUploadVoD(),
		streamVersion: "COMB",
		isSelfStream:  false,
		ingestServer:  request.IngestServer,
		sourceUrl:     "rtmp://localhost/" + slug,
		streamName:    request.StreamName,
		outUrl:        request.OutUrl,
	}
	stream(streamCtx)
	return streamCtx
}

func HandleSelfStreamRecordEnd(ctx *StreamContext) {
	S.startTranscoding(ctx.getStreamName())
	err := transcode(ctx)
	if err != nil {
		ctx.TranscodingSuccessful = false
		NotifyTranscodingFailure(*ctx, err)
		log.Errorf("Error while transcoding: %v", err)
	} else {
		ctx.TranscodingSuccessful = true
	}
	S.endTranscoding(ctx.getStreamName())
	if ctx.canceled {
		// self stream restarted while transcoding
		return
	}
	notifyTranscodingDone(ctx)
	if ctx.publishVoD {
		upload(ctx)
		notifyUploadDone(ctx)
	}

	S.startThumbnailGeneration(ctx)
	defer S.endThumbnailGeneration(ctx)
	err = createThumbnailSprite(ctx, ctx.getTranscodingFileName())
	thumbSuccessful := true
	if err != nil {
		log.WithField("File", ctx.getThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail sprite failed.")
		thumbSuccessful = false
	}
	err = createVideoThumbnail(ctx, ctx.getTranscodingFileName())
	if err != nil {
		log.WithField("File", ctx.getLargeThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail failed.")
		thumbSuccessful = false
	}
	if thumbSuccessful {
		notifyThumbnailDone(ctx)
	}

	S.startSilenceDetection(ctx)
	defer S.endSilenceDetection(ctx)

	sd := NewSilenceDetector(ctx.getTranscodingFileName())
	err = sd.ParseSilence()
	if err != nil {
		log.WithField("File", ctx.getTranscodingFileName()).WithError(err).Error("Detecting silence failed.")
		return
	}
	notifySilenceResults(sd.Silences, ctx.streamId)

	if ctx.TranscodingSuccessful {
		err := markForDeletion(ctx)
		if err != nil {
			log.WithField("stream", ctx.streamId).WithError(err).Error("Error marking for deletion")
		}
	}
}

// HandleThumbnailRequest creates a thumbnail on demand.
func HandleThumbnailRequest(request *pb.GenerateThumbnailRequest) {
	streamCtx := &StreamContext{
		streamId:      request.StreamID,
		courseSlug:    request.CourseSlug,
		teachingTerm:  request.TeachingTerm,
		teachingYear:  request.CourseYear,
		startTime:     request.Start.AsTime(),
		streamVersion: request.StreamVersion,
		recordingPath: &request.Path,
	}
	if streamCtx.recordingPath == nil {
		return
	}
	S.startThumbnailGeneration(streamCtx)
	defer S.endThumbnailGeneration(streamCtx)
	err := createThumbnailSprite(streamCtx, *streamCtx.recordingPath)
	thumbSuccessful := true
	if err != nil {
		log.WithField("File", streamCtx.getThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail sprite failed.")
		thumbSuccessful = false
	}
	err = createVideoThumbnail(streamCtx, streamCtx.getTranscodingFileName())
	if err != nil {
		log.WithField("File", streamCtx.getLargeThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail failed.")
		thumbSuccessful = false
	}
	if thumbSuccessful {
		notifyThumbnailDone(streamCtx)
	}
}

// HandleStreamEndRequest ends all streams for a given streamID contained in request
func HandleStreamEndRequest(request *pb.EndStreamRequest) {
	log.Info("Attempting to end stream: ", request.StreamID)
	regularStreams.endStreams(request)
}

func (s *safeStreams) endStreams(request *pb.EndStreamRequest) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	stream := s.streams[request.StreamID]
	for _, streamContext := range stream {
		streamContext.discardVoD = request.DiscardVoD
		HandleStreamEnd(streamContext, false)
	}
	// All streams should be ended right now, so we can delete them
	delete(s.streams, request.StreamID)
}

// HandleStreamEnd stops the ffmpeg instance by sending a SIGINT to it and prevents the loop to restart it by marking the stream context as stopped.
func HandleStreamEnd(ctx *StreamContext, cancelTranscoding bool) {
	ctx.stopped = true
	cancelCmdGroup(ctx.streamCmd)
	if cancelTranscoding {
		ctx.publishVoD = false
		ctx.canceled = true
		cancelCmd(ctx.transcodingCmd)
	}
}

func cancelCmdGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		log.Info("Sending SIGINT to pgid: ", -cmd.Process.Pid)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // avoid suicide when killing group (assign new group id)
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if err != nil {
			log.WithError(err).WithField("cmd", cmd.String()).Warn("can't kill command group")
		}
	} else {
		log.Warn("context has no command or process to end")
	}
}

func cancelCmd(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		log.Info("Sending SIGINT to pid: ", cmd.Process.Pid)
		err := cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			log.WithError(err).WithField("cmd", cmd.String()).Warn("Can't interrupt ffmpeg")
		}
	} else {
		log.Warn("context has no command or process to end")
	}
}

func HandleStreamRequest(request *pb.StreamRequest) {
	log.WithField("request", request).Info("Request to stream")
	//setup context with relevant information to pass to other subprocesses
	streamCtx := &StreamContext{
		streamId:      request.GetStreamID(),
		sourceUrl:     "rtsp://" + request.GetSourceUrl(),
		courseSlug:    request.GetCourseSlug(),
		teachingTerm:  request.GetCourseTerm(),
		teachingYear:  request.GetCourseYear(),
		startTime:     request.GetStart().AsTime().Local(),
		endTime:       request.GetEnd().AsTime().Local(),
		streamVersion: request.GetSourceType(),
		publishVoD:    request.GetPublishVoD(),
		streamName:    request.GetStreamName(),
		ingestServer:  request.GetIngestServer(),
		isSelfStream:  false,
		outUrl:        request.GetOutUrl(),
	}

	// Register worker for stream
	regularStreams.addContext(streamCtx.streamId, streamCtx)

	stream(streamCtx)

	NotifyStreamDone(streamCtx) // notify stream/recording done
	if streamCtx.discardVoD {
		log.Info("Skipping VoD creation")
		return
	}
	S.startTranscoding(streamCtx.getStreamName())
	err := transcode(streamCtx)
	if err != nil {
		streamCtx.TranscodingSuccessful = false
		NotifyTranscodingFailure(*streamCtx, err)
		log.Errorf("Error while transcoding: %v", err)
	} else {
		streamCtx.TranscodingSuccessful = true
	}
	S.endTranscoding(streamCtx.getStreamName())
	notifyTranscodingDone(streamCtx)

	if streamCtx.streamVersion == "COMB" {
		err = transcodeAudio(streamCtx)
		if err != nil {
			log.WithError(err).Error("Error transcoding audio")
		}
	}


	S.startThumbnailGeneration(streamCtx)
	defer S.endThumbnailGeneration(streamCtx)
	err = createThumbnailSprite(streamCtx, streamCtx.getTranscodingFileName())
	thumbSuccessful := true
	if err != nil {
		log.WithField("File", streamCtx.getThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail sprite failed")
		thumbSuccessful = false
	}
	err = createVideoThumbnail(streamCtx, streamCtx.getTranscodingFileName())
	if err != nil {
		log.WithField("File", streamCtx.getLargeThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail failed.")
		thumbSuccessful = false
	}
	if thumbSuccessful {
		notifyThumbnailDone(streamCtx)
	}

	if request.PublishVoD {
		upload(streamCtx)
		notifyUploadDone(streamCtx)
	}

	if streamCtx.streamVersion == "COMB" {
		S.startSilenceDetection(streamCtx)
		defer S.endSilenceDetection(streamCtx)
		sd := NewSilenceDetector(streamCtx.getTranscodingFileName())
		err := sd.ParseSilence()
		if err != nil {
			log.WithField("File", streamCtx.getTranscodingFileName()).WithError(err).Error("Detecting silence failed.")
			return
		}
		notifySilenceResults(sd.Silences, streamCtx.streamId)
	}
	if streamCtx.TranscodingSuccessful {
		err := markForDeletion(streamCtx)
		if err != nil {
			log.WithField("stream", streamCtx.streamId).WithError(err).Error("Error marking for deletion")
		}
	}
}

func GetStreamInfoForUploadReq(uploadKey string) (*pb.GetStreamInfoForUploadResponse, error) {
	client, conn, err := GetClient()
	if err != nil {
		return nil, err
	}
	defer closeConnection(conn)
	return client.GetStreamInfoForUpload(context.Background(), &pb.GetStreamInfoForUploadRequest{
		UploadKey: uploadKey,
		WorkerID:  cfg.WorkerID,
	})
}

func HandleUploadRestReq(streamInfo *pb.GetStreamInfoForUploadResponse, localFile string) {
	c := StreamContext{
		streamId:      streamInfo.StreamID,
		courseSlug:    streamInfo.CourseSlug,
		teachingTerm:  streamInfo.CourseTerm,
		teachingYear:  streamInfo.CourseYear,
		startTime:     streamInfo.StreamStart.AsTime(),
		endTime:       streamInfo.StreamEnd.AsTime(),
		streamVersion: streamInfo.VideoType,
		publishVoD:    true,
		recordingPath: &localFile,
	}
	log.WithFields(log.Fields{"stream": c.streamId, "course": c.courseSlug, "file": localFile}).Debug("Handling upload request")

	needsConversion := false

	if container, err := getContainer(localFile); err != nil {
		log.WithError(err).Error("Error getting container")
		needsConversion = true
	} else if !strings.Contains(container, "mp4") {
		needsConversion = true
		log.Debugf("Wrong container: %s, converting", container)
	}

	if codec, err := getCodec(localFile); err != nil {
		log.WithError(err).Warn("Error getting codec")
		needsConversion = true
	} else if codec != "h264" {
		needsConversion = true
		log.Debugf("wrong codec: %s, converting", codec)
	}

	level, err := getLevel(localFile)
	if err != nil {
		log.WithError(err).Warn("Error getting level")
		needsConversion = true
	}
	if levelInt, err := strconv.Atoi(level); err != nil {
		log.WithError(err).Warnf("Error converting level(%s) to int", level)
		needsConversion = true
	} else {
		if levelInt > 42 {
			needsConversion = true
			log.Debugf("Level too high: %d, converting", levelInt)
		}
	}

	if needsConversion {
		log.WithField("stream", c.streamId).Debug("Converting video from upload request")
		S.startTranscoding(c.getStreamName())
		err := transcode(&c)
		if err != nil {
			NotifyTranscodingFailure(c, err)
			log.WithError(err).Error("Error transcoding")
		}
		notifyTranscodingDone(&c)
		S.endTranscoding(c.getStreamName())

	} else {
		log.WithField("stream", c.streamId).Debug("Not converting video from upload request")
		// create required directories
		log.WithField("transcodingFileName", c.getTranscodingFileName()).Debug("Creating output directory")
		if err = prepare(c.getTranscodingFileName()); err != nil {
			log.Error(err)
		}
		log.WithFields(log.Fields{"in": c.getRecordingTrashName(), "out": c.getTranscodingFileName()}).Debug("Copying file")
		if err = moveFile(c.getRecordingFileName(), c.getTranscodingFileName()); err != nil {
			log.WithError(err).Error("Can't move upload to transcoding dir")
		} else {
			log.WithField("stream", c.streamId).Debug("Successfully moved upload to target dir")
		}
	}

	err = transcodeAudio(&c)
	if err != nil {
		log.WithError(err).Error("Error transcoding audio")
	}

	S.startThumbnailGeneration(&c)
	defer S.endThumbnailGeneration(&c)
	err = createThumbnailSprite(&c, c.getTranscodingFileName())
	thumbSuccessful := true
	if err != nil {
		log.WithField("File", c.getThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail sprite failed.")
		thumbSuccessful = false
	}
	err = createVideoThumbnail(&c, c.getTranscodingFileName())
	if err != nil {
		log.WithField("File", c.getLargeThumbnailSpriteFileName()).WithError(err).Error("Creating thumbnail failed.")
		thumbSuccessful = false
	}
	if thumbSuccessful {
		notifyThumbnailDone(&c)
	}

	S.startSilenceDetection(&c)
	defer S.endSilenceDetection(&c)
	sd := NewSilenceDetector(c.getTranscodingFileName())
	if err = sd.ParseSilence(); err != nil {
		log.WithField("File", c.getTranscodingFileName()).WithError(err).Error("Detecting silence failed.")
	} else {
		notifySilenceResults(sd.Silences, c.streamId)
	}

	upload(&c)
	notifyUploadDone(&c)
	_ = os.Remove(c.getRecordingFileName())
}

// moveFile moves a file from sourcePath to destPath.
// in contrast to os.Rename, this function can copy files from one drive to another.
func moveFile(sourcePath, destPath string) error {
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source file: %s", err)
	}
	outputFile, err := os.Create(destPath)
	if err != nil {
		inputFile.Close()
		return fmt.Errorf("open dest file: %s", err)
	}
	defer outputFile.Close()
	_, err = io.Copy(outputFile, inputFile)
	inputFile.Close()
	if err != nil {
		return fmt.Errorf("writing to output file: %s", err)
	}
	// The copy was successful, so now delete the original file
	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("removing original file: %s", err)
	}
	return nil
}

// StreamContext contains all important information on a stream
type StreamContext struct {
	streamId       uint32    //id of the stream
	sourceUrl      string    //url of the streams source, e.g. 10.0.0.4
	courseSlug     string    //slug of the course, e.g. eidi
	teachingTerm   string    //S or W depending on the courses teaching-term
	teachingYear   uint32    //Year the course takes place in
	startTime      time.Time //time the stream should start
	endTime        time.Time //end of the stream (including +10 minute safety)
	streamVersion  string    //version of the stream to be handled, e.g. PRES, COMB or CAM
	publishVoD     bool      //whether file should be uploaded
	streamCmd      *exec.Cmd // command used for streaming
	transcodingCmd *exec.Cmd // command used for transcoding
	isSelfStream   bool      //deprecated
	canceled       bool      // selfstreams are canceled when the same stream starts again.

	streamName   string // ingest target
	ingestServer string // ingest tumlive e.g. rtmp://user:password@my.tumlive
	stopped      bool   // whether the stream has been stopped
	outUrl       string // url the stream will be available at
	discardVoD   bool   // whether the VoD should be discarded

	// calculated after stream:
	duration      uint32 //duration of the stream in seconds
	thumbInterval uint32 // interval between thumbnails in seconds

	TranscodingSuccessful bool // TranscodingSuccessful is true if the transcoding was successful

	thumbnailSpritePath string  // path to the thumbnail sprite
	recordingPath       *string // recordingPath: path to the recording (overrides default path if set)
}

// getRecordingFileName returns the filename a stream should be saved to before transcoding.
// example: /recordings/eidi_2021-09-23_10-00_COMB.ts
func (s StreamContext) getRecordingFileName() string {
	if s.recordingPath != nil {
		return *s.recordingPath
	}
	if !s.isSelfStream {
		return fmt.Sprintf("%s/%s.ts",
			cfg.TempDir,
			s.getStreamName())
	}
	return fmt.Sprintf("%s/%s_%s.flv",
		cfg.TempDir,
		s.courseSlug,
		s.startTime.Format("02012006"))
}

func (s StreamContext) getRecordingTrashName() string {
	fn := s.getRecordingFileName()
	return filepath.Join(filepath.Dir(fn), ".trash", filepath.Base(fn))
}

// getTranscodingFileName returns the filename a stream should be saved to after transcoding.
// example: /srv/sharedMassStorage/2021/S/eidi/2021-09-23_10-00/eidi_2021-09-23_10-00_PRES.mp4
func (s StreamContext) getTranscodingFileName() string {
	if s.isSelfStream {
		return fmt.Sprintf("%s/%d/%s/%s/%s/%s-%s.mp4",
			cfg.StorageDir,
			s.teachingYear,
			s.teachingTerm,
			s.courseSlug,
			s.startTime.Format("2006-01-02_15-04"),
			s.courseSlug,
			s.startTime.Format("02012006"))
	}
	return fmt.Sprintf("%s/%d/%s/%s/%s/%s.mp4",
		cfg.StorageDir,
		s.teachingYear,
		s.teachingTerm,
		s.courseSlug,
		s.startTime.Format("2006-01-02_15-04"),
		s.getStreamName())
}

func (s StreamContext) getAudioTranscodingFileName() string {
	return fmt.Sprintf("%s/%d/%s/%s/%d.m4a",
		cfg.StorageDir,
		s.teachingYear,
		s.teachingTerm,
		s.courseSlug,
		s.streamId)
}

// getLargeThumbnailSpriteFileName returns the path the full thumbnail sprite should be saved to after transcoding.
// example: /srv/sharedMassStorage/2021/S/eidi/2021-09-23_10-00/eidi_2021-09-23_10-00_PRES-thumb.jpg
func (s StreamContext) getLargeThumbnailSpriteFileName() string {
	return fmt.Sprintf("%s/%d/%s/%s/%d-thumb-full-%s.jpg",
		cfg.StorageDir,
		s.teachingYear,
		s.teachingTerm,
		s.courseSlug,
		s.streamId,
		s.streamVersion)
}

// getThumbnailSpriteFileName returns the path a thumbnail sprite should be saved to after transcoding.
// example: /srv/sharedMassStorage/2021/S/eidi/2021-09-23_10-00/eidi_2021-09-23_10-00_PRES-thumb.jpg
func (s StreamContext) getThumbnailSpriteFileName() string {
	if s.isSelfStream {
		return fmt.Sprintf("%s/%d/%s/%s/%s/%s-%s-thumb.jpg",
			cfg.StorageDir,
			s.teachingYear,
			s.teachingTerm,
			s.courseSlug,
			s.startTime.Format("2006-01-02_15-04"),
			s.courseSlug,
			s.startTime.Format("02012006"))
	}
	return fmt.Sprintf("%s/%d/%s/%s/%s/%s-thumb.jpg",
		cfg.StorageDir,
		s.teachingYear,
		s.teachingTerm,
		s.courseSlug,
		s.startTime.Format("2006-01-02_15-04"),
		s.getStreamName())
}

// getStreamName returns the stream name, used for the worker status
func (s StreamContext) getStreamName() string {
	if !s.isSelfStream {
		return fmt.Sprintf("%s-%s%s",
			s.courseSlug,
			s.startTime.Format("2006-01-02-15-04"),
			s.streamVersion)
	}
	return s.courseSlug
}

var vodFileNameIllegal = regexp.MustCompile(`[^a-zA-Z0-9_\\.]+`)

// getStreamNameVoD returns the stream name for vod (lrz replaces - with _)
func (s StreamContext) getStreamNameVoD() string {
	if !s.isSelfStream {
		name := strings.ReplaceAll(fmt.Sprintf("%s_%s%s",
			s.courseSlug,
			s.startTime.Format("2006_01_02_15_04"),
			s.streamVersion), "-", "_")
		return vodFileNameIllegal.ReplaceAllString(name, "_")
	}
	return s.courseSlug
}
