package wechat

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// defaultCDNBaseURL is the WeChat c2c CDN host used for all media downloads.
// It is separate from the iLink API host (ilinkai.weixin.qq.com).
const defaultCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

// Downloader fetches, decrypts, and saves inbound media from the iLink CDN.
type Downloader struct {
	cdnBaseURL string
	mediaDir   string
	httpClient *http.Client
}

// NewDownloader creates a Downloader. cdnBaseURL defaults to the WeChat c2c CDN
// if empty. mediaDir is where downloaded files are written; it is resolved to
// an absolute path so that file paths injected into agent messages remain valid
// when the agent runs in a different working directory.
func NewDownloader(cdnBaseURL, mediaDir string) *Downloader {
	if cdnBaseURL == "" {
		cdnBaseURL = defaultCDNBaseURL
	}
	if abs, err := filepath.Abs(mediaDir); err == nil {
		mediaDir = abs
	}
	return &Downloader{
		cdnBaseURL: cdnBaseURL,
		mediaDir:   mediaDir,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// DownloadImage downloads, decrypts, and saves an image item.
// Returns the absolute local file path.
func (d *Downloader) DownloadImage(ctx context.Context, item MessageItem) (string, error) {
	img := item.ImageItem
	if img == nil || img.Media == nil || img.Media.EncryptQueryParam == "" {
		return "", fmt.Errorf("image item missing media ref")
	}

	var data []byte
	var err error

	switch {
	case img.AESKey != "":
		// aeskey is a hex-encoded 16-byte key — convert to base64 before parsing.
		keyBase64 := hexStringToBase64(img.AESKey)
		data, err = d.downloadAndDecrypt(ctx, img.Media.EncryptQueryParam, keyBase64, "image")
	case img.Media.AESKey != "":
		data, err = d.downloadAndDecrypt(ctx, img.Media.EncryptQueryParam, img.Media.AESKey, "image")
	default:
		data, err = d.fetchCDN(ctx, img.Media.EncryptQueryParam)
	}
	if err != nil {
		return "", err
	}
	return d.save(data, "image-"+uuid.New().String()[:8]+sniffImageExt(data))
}

// DownloadFile downloads, decrypts, and saves a file item.
// Returns the absolute local file path.
func (d *Downloader) DownloadFile(ctx context.Context, item MessageItem) (string, error) {
	f := item.FileItem
	if f == nil || f.Media == nil || f.Media.EncryptQueryParam == "" || f.Media.AESKey == "" {
		return "", fmt.Errorf("file item missing media ref or aes key")
	}
	data, err := d.downloadAndDecrypt(ctx, f.Media.EncryptQueryParam, f.Media.AESKey, "file")
	if err != nil {
		return "", err
	}
	name := f.FileName
	if name == "" {
		name = "file-" + uuid.New().String()[:8] + ".bin"
	}
	return d.save(data, name)
}

// DownloadVoice downloads, decrypts, and saves a voice item.
// Returns the absolute local file path. WeChat voice messages are typically
// encoded as SILK audio (encode_type=6).
func (d *Downloader) DownloadVoice(ctx context.Context, item MessageItem) (string, error) {
	v := item.VoiceItem
	if v == nil || v.Media == nil || v.Media.EncryptQueryParam == "" || v.Media.AESKey == "" {
		return "", fmt.Errorf("voice item missing media ref or aes key")
	}
	data, err := d.downloadAndDecrypt(ctx, v.Media.EncryptQueryParam, v.Media.AESKey, "voice")
	if err != nil {
		return "", err
	}
	return d.save(data, "voice-"+uuid.New().String()[:8]+".silk")
}

// DownloadVideo downloads, decrypts, and saves a video item.
// Returns the absolute local file path.
func (d *Downloader) DownloadVideo(ctx context.Context, item MessageItem) (string, error) {
	v := item.VideoItem
	if v == nil || v.Media == nil || v.Media.EncryptQueryParam == "" || v.Media.AESKey == "" {
		return "", fmt.Errorf("video item missing media ref or aes key")
	}
	data, err := d.downloadAndDecrypt(ctx, v.Media.EncryptQueryParam, v.Media.AESKey, "video")
	if err != nil {
		return "", err
	}
	return d.save(data, "video-"+uuid.New().String()[:8]+".mp4")
}

func (d *Downloader) downloadAndDecrypt(ctx context.Context, encryptedQueryParam, aesKeyBase64, label string) ([]byte, error) {
	key, err := parseAESKey(aesKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("%s: parse aes key: %w", label, err)
	}
	encrypted, err := d.fetchCDN(ctx, encryptedQueryParam)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	plaintext, err := decryptAES128ECB(encrypted, key)
	if err != nil {
		return nil, fmt.Errorf("%s: decrypt: %w", label, err)
	}
	slog.Debug("wechat: media decrypted", "label", label, "bytes", len(plaintext))
	return plaintext, nil
}

func (d *Downloader) fetchCDN(ctx context.Context, encryptedQueryParam string) ([]byte, error) {
	cdnURL := d.cdnBaseURL + "/download?encrypted_query_param=" + url.QueryEscape(encryptedQueryParam)
	slog.Debug("wechat: cdn fetch", "url_prefix", cdnURL[:min(len(cdnURL), 80)])

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdn build request: %w", err)
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn fetch: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (d *Downloader) save(data []byte, name string) (string, error) {
	if err := os.MkdirAll(d.mediaDir, 0755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	path := filepath.Join(d.mediaDir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("write media file: %w", err)
	}
	slog.Info("wechat: media saved", "path", path, "bytes", len(data))
	return path, nil
}

// parseAESKey decodes a base64-encoded AES-128 key. Handles two wire formats:
//   - base64(16 raw bytes)       — used by image Media.AESKey
//   - base64(32-char hex string) — used by voice/file/video Media.AESKey
func parseAESKey(aesKeyBase64 string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(aesKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(decoded) == 16 {
		return decoded, nil
	}
	if len(decoded) == 32 && hexRe.Match(decoded) {
		raw, err := hex.DecodeString(string(decoded))
		if err == nil && len(raw) == 16 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("aes key decodes to %d bytes, expected 16", len(decoded))
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// hexStringToBase64 converts a hex-encoded key (image_item.aeskey) to base64.
func hexStringToBase64(hexKey string) string {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		// Not valid hex; treat the string bytes as the key directly.
		return base64.StdEncoding.EncodeToString([]byte(hexKey))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// decryptAES128ECB decrypts ciphertext with AES-128-ECB + PKCS7 unpadding.
func decryptAES128ECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not a non-zero multiple of block size", len(data))
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return pkcs7Unpad(out)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, fmt.Errorf("invalid pkcs7 padding %d", pad)
	}
	return data[:len(data)-pad], nil
}

// encryptAES128ECB encrypts plaintext with AES-128-ECB + PKCS7 padding.
func encryptAES128ECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	// PKCS7 pad to block boundary
	padLen := aes.BlockSize - (len(data) % aes.BlockSize)
	padded := make([]byte, len(data)+padLen)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return out, nil
}

// aesECBPaddedSize returns the AES-128-ECB ciphertext size for a given plaintext
// length, matching the TypeScript aesEcbPaddedSize: ceil((n+1)/16)*16.
func aesECBPaddedSize(n int) int {
	return ((n + 1 + aes.BlockSize - 1) / aes.BlockSize) * aes.BlockSize
}

// UploadedImage holds the result of a successful image CDN upload.
type UploadedImage struct {
	DownloadParam  string // encrypt_query_param for the sendmessage image_item
	AESKeyHex      string // hex-encoded 16-byte AES key
	CiphertextSize int    // encrypted byte length (for mid_size field)
}

// UploadImage encrypts imageData and uploads it to the iLink CDN via the
// provided client. toUserID is required by the getuploadurl API.
func UploadImage(ctx context.Context, client *Client, cdnBaseURL, toUserID string, imageData []byte) (*UploadedImage, error) {
	// Generate random 16-byte AES key and filekey.
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}
	filekeyRaw := make([]byte, 16)
	if _, err := rand.Read(filekeyRaw); err != nil {
		return nil, fmt.Errorf("generate filekey: %w", err)
	}
	aeskeyHex := hex.EncodeToString(keyRaw)
	filekey := hex.EncodeToString(filekeyRaw)

	rawMD5 := md5.Sum(imageData)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])
	cipherSize := aesECBPaddedSize(len(imageData))

	slog.Debug("wechat: upload image", "rawsize", len(imageData), "ciphersize", cipherSize, "md5", rawMD5Hex)

	uploadResp, err := client.GetUploadUrl(ctx, GetUploadUrlReq{
		FileKey:    filekey,
		MediaType:  UploadMediaTypeImage,
		ToUserID:   toUserID,
		RawSize:    len(imageData),
		RawFileMD5: rawMD5Hex,
		FileSize:   cipherSize,
		NoNeedThumb: true,
		AESKey:     aeskeyHex,
	})
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}

	encrypted, err := encryptAES128ECB(imageData, keyRaw)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	downloadParam, err := client.UploadToCDN(ctx, cdnBaseURL, uploadResp.UploadParam, filekey, encrypted)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}

	return &UploadedImage{
		DownloadParam:  downloadParam,
		AESKeyHex:      aeskeyHex,
		CiphertextSize: cipherSize,
	}, nil
}

// UploadedFile holds the result of a successful file CDN upload.
type UploadedFile struct {
	DownloadParam  string // encrypt_query_param for the sendmessage file_item
	AESKeyHex      string // hex-encoded 16-byte AES key
	CiphertextSize int    // encrypted byte length
	RawMD5Hex      string // MD5 of the plaintext data
}

// UploadFile encrypts fileData and uploads it to the iLink CDN as a file attachment.
// toUserID is required by the getuploadurl API.
func UploadFile(ctx context.Context, client *Client, cdnBaseURL, toUserID string, fileData []byte) (*UploadedFile, error) {
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}
	filekeyRaw := make([]byte, 16)
	if _, err := rand.Read(filekeyRaw); err != nil {
		return nil, fmt.Errorf("generate filekey: %w", err)
	}
	aeskeyHex := hex.EncodeToString(keyRaw)
	filekey := hex.EncodeToString(filekeyRaw)

	rawMD5 := md5.Sum(fileData)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])
	cipherSize := aesECBPaddedSize(len(fileData))

	slog.Debug("wechat: upload file", "rawsize", len(fileData), "ciphersize", cipherSize, "md5", rawMD5Hex)

	uploadResp, err := client.GetUploadUrl(ctx, GetUploadUrlReq{
		FileKey:    filekey,
		MediaType:  UploadMediaTypeFile,
		ToUserID:   toUserID,
		RawSize:    len(fileData),
		RawFileMD5: rawMD5Hex,
		FileSize:   cipherSize,
		AESKey:     aeskeyHex,
	})
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}

	encrypted, err := encryptAES128ECB(fileData, keyRaw)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	downloadParam, err := client.UploadToCDN(ctx, cdnBaseURL, uploadResp.UploadParam, filekey, encrypted)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}

	return &UploadedFile{
		DownloadParam:  downloadParam,
		AESKeyHex:      aeskeyHex,
		CiphertextSize: cipherSize,
		RawMD5Hex:      rawMD5Hex,
	}, nil
}

// sniffImageExt returns a file extension inferred from magic bytes.
func sniffImageExt(data []byte) string {
	if len(data) < 4 {
		return ".bin"
	}
	switch {
	case data[0] == 0xFF && data[1] == 0xD8:
		return ".jpg"
	case data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47:
		return ".png"
	case data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46:
		return ".gif"
	case len(data) >= 12 && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ".jpg"
}
