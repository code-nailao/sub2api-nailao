package service

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Standalone tool operations. Search and Fetch are also used by the hosted loop.
const (
	DeepSeekToolSearch = "search"
	DeepSeekToolFetch  = "fetch"
	DeepSeekToolOCR    = "ocr"
	DeepSeekToolASR    = "asr"
	DeepSeekToolTTS    = "tts"
)

// Request limits, all checked before any supplier call.
const (
	deepSeekJSONToolMaxBody  = 1 << 20
	deepSeekTTSMaxBody       = 64 << 10
	deepSeekTTSMaxCharacters = 10000
	deepSeekASRMaxFile       = 8 << 20
	deepSeekASRMaxBody       = 9 << 20
	deepSeekASRMaxSeconds    = 120
	deepSeekOCRMaxFile       = 100 << 20
	deepSeekOCRMaxBody       = 101 << 20
	deepSeekOCRMaxPages      = 1000
	// deepSeekASRMinBytesPerSecond bounds what an ASR answer may claim: no
	// accepted audio is denser in time than 6 kbit/s, the lowest Opus bitrate
	// (16-bit PCM needs at least 16 kB/s), so a file of n bytes holds at most
	// n/750 seconds whatever its header declares.
	deepSeekASRMinBytesPerSecond = 750
	// Leading bytes kept for type detection.
	deepSeekFileHeadBytes = 1024
)

// IsDeepSeekToolOperation reports whether operation is one of the five tools.
func IsDeepSeekToolOperation(operation string) bool {
	switch operation {
	case DeepSeekToolSearch, DeepSeekToolFetch, DeepSeekToolOCR, DeepSeekToolASR, DeepSeekToolTTS:
		return true
	}
	return false
}

// DeepSeekToolMaxBody is the request body cap for one standalone tool.
func DeepSeekToolMaxBody(operation string) int64 {
	switch operation {
	case DeepSeekToolOCR:
		return deepSeekOCRMaxBody
	case DeepSeekToolASR:
		return deepSeekASRMaxBody
	case DeepSeekToolTTS:
		return deepSeekTTSMaxBody
	}
	return deepSeekJSONToolMaxBody
}

// DeepSeekToolRequest is a validated standalone tool request.
type DeepSeekToolRequest struct {
	Operation string
	// Body is sent to the supplier unchanged: normalized JSON for the JSON
	// tools, the checked multipart body for OCR and ASR.
	Body        []byte
	ContentType string
	// AuditText is the text passed to the content audit ("" for files).
	AuditText string
	// EstimateUnits sizes the in-flight reservation: one call, 120 seconds of
	// audio, the actual billable characters, or the page maximum unless the
	// file is a single image.
	EstimateUnits int
	// MaxUnits is the most a supplier answer can be charged for: the documented
	// page maximum for OCR, what the file size allows for ASR, the request's own
	// units otherwise.
	MaxUnits int
}

// DeepSeekToolInputError is a request the gateway refuses without calling the supplier.
type DeepSeekToolInputError struct {
	Status  int
	Message string
}

func (e *DeepSeekToolInputError) Error() string { return e.Message }

func deepSeekInputError(format string, args ...any) error {
	return &DeepSeekToolInputError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

// PrepareDeepSeekToolRequest validates a standalone request. Only the
// supplier's documented fields are accepted, so a caller cannot inject a model,
// credential or upstream URL.
func PrepareDeepSeekToolRequest(operation, contentType string, body []byte) (*DeepSeekToolRequest, error) {
	if int64(len(body)) > DeepSeekToolMaxBody(operation) {
		return nil, &DeepSeekToolInputError{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("request body exceeds %d bytes", DeepSeekToolMaxBody(operation))}
	}
	switch operation {
	case DeepSeekToolSearch, DeepSeekToolFetch:
		normalized, err := ValidateDeepSeekToolRequest(operation, body)
		if err != nil {
			return nil, &DeepSeekToolInputError{Status: http.StatusBadRequest, Message: err.Error()}
		}
		field := "query"
		if operation == DeepSeekToolFetch {
			field = "url"
		}
		var fields map[string]any
		_ = json.Unmarshal(normalized, &fields)
		audit, _ := fields[field].(string)
		return &DeepSeekToolRequest{Operation: operation, Body: normalized, ContentType: "application/json", AuditText: audit, EstimateUnits: 1, MaxUnits: 1}, nil
	case DeepSeekToolTTS:
		return prepareDeepSeekTTS(body)
	case DeepSeekToolASR, DeepSeekToolOCR:
		return prepareDeepSeekUpload(operation, contentType, body)
	}
	return nil, &DeepSeekToolInputError{Status: http.StatusNotFound, Message: "unsupported DeepSeek tool"}
}

// DeepSeekTTSBillableCharacters counts a Han character as 2 and any other
// character as 1, the supplier's unit for speech synthesis.
func DeepSeekTTSBillableCharacters(text string) int {
	count := 0
	for _, r := range text {
		count++
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}
	return count
}

func prepareDeepSeekTTS(body []byte) (*DeepSeekToolRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, deepSeekInputError("tool request must be a JSON object")
	}
	out := map[string]string{}
	for k, raw := range fields {
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return nil, deepSeekInputError("%s must be a string", k)
		}
		switch k {
		case "text":
			if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
				return nil, deepSeekInputError("text must not be empty")
			}
		case "voice_id":
			if strings.TrimSpace(value) == "" || len(value) > 128 {
				return nil, deepSeekInputError("voice_id must contain 1 to 128 bytes")
			}
		case "format":
			if value != "wav" && value != "pcm" {
				return nil, deepSeekInputError("format must be wav or pcm")
			}
		default:
			return nil, deepSeekInputError("unsupported tts field: %s", k)
		}
		out[k] = value
	}
	text, ok := out["text"]
	if !ok {
		return nil, deepSeekInputError("text is required")
	}
	chars := DeepSeekTTSBillableCharacters(text)
	if chars > deepSeekTTSMaxCharacters {
		return nil, deepSeekInputError("text exceeds %d billable characters (a Han character counts as 2)", deepSeekTTSMaxCharacters)
	}
	normalized, err := json.Marshal(out)
	if err != nil {
		return nil, deepSeekInputError("invalid tts request")
	}
	return &DeepSeekToolRequest{Operation: DeepSeekToolTTS, Body: normalized, ContentType: "application/json", AuditText: text, EstimateUnits: chars, MaxUnits: chars}, nil
}

// prepareDeepSeekUpload checks a multipart upload without copying it: only the
// documented parts may be present, exactly once each, and the file must be of
// an accepted type within the size and duration limits. The checked body is
// then forwarded byte for byte with a normalized Content-Type.
func prepareDeepSeekUpload(operation, contentType string, body []byte) (*DeepSeekToolRequest, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, &DeepSeekToolInputError{Status: http.StatusUnsupportedMediaType, Message: "Content-Type must be multipart/form-data"}
	}
	maxFile := int64(deepSeekASRMaxFile)
	if operation == DeepSeekToolOCR {
		maxFile = deepSeekOCRMaxFile
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var file *deepSeekFileInspector
	purposeSeen := false
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, deepSeekInputError("malformed multipart body")
		}
		name := part.FormName()
		switch {
		case name == "file" && file == nil:
			if part.FileName() == "" {
				return nil, deepSeekInputError("file must be uploaded as a file")
			}
			// OCR files are inspected while streaming so a 100 MiB upload is
			// not copied; ASR keeps its (at most 8 MiB) audio to read the duration.
			file = &deepSeekFileInspector{keep: operation == DeepSeekToolASR}
			if _, readErr := io.Copy(file, io.LimitReader(part, maxFile+1)); readErr != nil {
				return nil, deepSeekInputError("malformed multipart body")
			}
			if file.size > maxFile {
				return nil, &DeepSeekToolInputError{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("file exceeds %d bytes", maxFile)}
			}
			if file.size == 0 {
				return nil, deepSeekInputError("file is empty")
			}
		case name == "purpose" && operation == DeepSeekToolOCR && !purposeSeen:
			value, readErr := io.ReadAll(io.LimitReader(part, 64))
			if readErr != nil || string(value) != "text_recognition" {
				return nil, deepSeekInputError("purpose must be text_recognition")
			}
			purposeSeen = true
		default:
			return nil, deepSeekInputError("unsupported or repeated %s field: %q", operation, name)
		}
	}
	if file == nil {
		return nil, deepSeekInputError("file is required")
	}
	if operation == DeepSeekToolOCR && !purposeSeen {
		return nil, deepSeekInputError("purpose must be text_recognition")
	}
	units, maxUnits := deepSeekASRMaxSeconds, int((file.size+deepSeekASRMinBytesPerSecond-1)/deepSeekASRMinBytesPerSecond)
	if operation == DeepSeekToolASR {
		// The declared length is checked here; the charge follows the supplier's
		// answer, which a forged header cannot lower.
		seconds, err := deepSeekAudioSeconds(file.data)
		if err != nil {
			return nil, deepSeekInputError("%s", err.Error())
		}
		if seconds > deepSeekASRMaxSeconds+0.05 {
			return nil, deepSeekInputError("audio exceeds %d seconds", deepSeekASRMaxSeconds)
		}
	} else {
		if deepSeekDocumentKind(file.head) == "" {
			return nil, deepSeekInputError("file must be a PDF, Word or PowerPoint document, or an image")
		}
		// Anything that may hold several pages is reserved at the maximum the
		// supplier accepts; the charge uses the page count it confirms.
		units, maxUnits = deepSeekOCRMaxPages, deepSeekOCRMaxPages
		if deepSeekSingleImage(file.head, file.size) {
			units = 1
		}
	}
	normalizedType := mime.FormatMediaType("multipart/form-data", map[string]string{"boundary": params["boundary"]})
	return &DeepSeekToolRequest{Operation: operation, Body: body, ContentType: normalizedType, EstimateUnits: units, MaxUnits: maxUnits}, nil
}

// deepSeekDocumentKind sniffs the accepted OCR inputs by their signature.
func deepSeekDocumentKind(file []byte) string {
	switch {
	case bytes.HasPrefix(file, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(file, []byte("PK\x03\x04")):
		return "office" // docx, pptx
	case bytes.HasPrefix(file, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}):
		return "office" // doc, ppt
	case bytes.HasPrefix(file, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(file, []byte{0xFF, 0xD8, 0xFF}),
		bytes.HasPrefix(file, []byte("GIF87a")), bytes.HasPrefix(file, []byte("GIF89a")),
		bytes.HasPrefix(file, []byte("BM")),
		bytes.HasPrefix(file, []byte("II*\x00")), bytes.HasPrefix(file, []byte("MM\x00*")),
		len(file) >= 12 && string(file[:4]) == "RIFF" && string(file[8:12]) == "WEBP":
		return "image"
	}
	return ""
}

// deepSeekSingleImage accepts only image formats that hold one frame, checked
// beyond their magic number, and nothing a lenient reader could take for a PDF.
// TIFF, GIF and extended WebP may hold several frames and are not included.
func deepSeekSingleImage(head []byte, size int64) bool {
	if bytes.Contains(head, []byte("%PDF")) {
		return false
	}
	switch {
	case len(head) >= 16 && bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return string(head[12:16]) == "IHDR"
	case len(head) >= 4 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return head[3] >= 0xC0 && head[3] != 0xFF // a segment marker follows SOI
	case len(head) >= 16 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return string(head[12:16]) == "VP8 " || string(head[12:16]) == "VP8L"
	case len(head) >= 18 && head[0] == 'B' && head[1] == 'M':
		// File size, reserved words and a known DIB header size must all agree.
		switch binary.LittleEndian.Uint32(head[14:18]) {
		case 12, 40, 52, 56, 64, 108, 124:
		default:
			return false
		}
		return int64(binary.LittleEndian.Uint32(head[2:6])) == size && binary.LittleEndian.Uint32(head[6:10]) == 0 &&
			int64(binary.LittleEndian.Uint32(head[10:14])) < size
	}
	return false
}

// deepSeekFileInspector receives an uploaded file once, keeping its size and
// leading bytes; ASR also keeps the (at most 8 MiB) audio to read its length.
type deepSeekFileInspector struct {
	keep bool
	data []byte
	head []byte
	size int64
}

func (f *deepSeekFileInspector) Write(p []byte) (int, error) {
	f.size += int64(len(p))
	if f.keep {
		f.data = append(f.data, p...)
	}
	if missing := deepSeekFileHeadBytes - len(f.head); missing > 0 {
		f.head = append(f.head, p[:min(missing, len(p))]...)
	}
	return len(p), nil
}

// deepSeekAudioSeconds reads the duration of a 16-bit PCM WAV or Ogg Opus file.
func deepSeekAudioSeconds(file []byte) (float64, error) {
	switch {
	case len(file) >= 12 && string(file[:4]) == "RIFF" && string(file[8:12]) == "WAVE":
		return deepSeekWAVSeconds(file)
	case bytes.HasPrefix(file, []byte("OggS")):
		return deepSeekOggOpusSeconds(file)
	}
	return 0, errors.New("audio must be 16-bit PCM WAV or Ogg Opus")
}

func deepSeekWAVSeconds(file []byte) (float64, error) {
	var byteRate uint32
	formatSeen := false
	for pos := 12; pos+8 <= len(file); {
		id := string(file[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(file[pos+4 : pos+8]))
		body := pos + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(file) {
				return 0, errors.New("invalid WAV header")
			}
			format := binary.LittleEndian.Uint16(file[body:])
			bits := binary.LittleEndian.Uint16(file[body+14:])
			byteRate = binary.LittleEndian.Uint32(file[body+8:])
			// 1 = PCM; 0xFFFE = WAVE_FORMAT_EXTENSIBLE, which carries PCM here.
			if (format != 1 && format != 0xFFFE) || bits != 16 || byteRate == 0 {
				return 0, errors.New("audio must be 16-bit PCM WAV or Ogg Opus")
			}
			formatSeen = true
		case "data":
			if !formatSeen {
				return 0, errors.New("invalid WAV header")
			}
			// Streaming writers may leave the size unset; use what is present.
			size = min(size, len(file)-body)
			return float64(size) / float64(byteRate), nil
		}
		if size < 0 || body+size < body {
			break
		}
		pos = body + size + size%2
	}
	return 0, errors.New("invalid WAV file")
}

// deepSeekOggOpusSeconds uses the final granule position (48 kHz) minus the
// pre-skip from the OpusHead packet.
func deepSeekOggOpusSeconds(file []byte) (float64, error) {
	head := bytes.Index(file, []byte("OpusHead"))
	if head < 0 || head+12 > len(file) {
		return 0, errors.New("audio must be 16-bit PCM WAV or Ogg Opus")
	}
	preSkip := int64(binary.LittleEndian.Uint16(file[head+10:]))
	last := bytes.LastIndex(file, []byte("OggS"))
	if last < 0 || last+14 > len(file) {
		return 0, errors.New("invalid Ogg file")
	}
	granule := int64(binary.LittleEndian.Uint64(file[last+6:]))
	if granule <= 0 {
		return 0, errors.New("invalid Ogg file")
	}
	return math.Max(0, float64(granule-preSkip)) / 48000, nil
}
