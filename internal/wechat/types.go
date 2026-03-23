package wechat

// Message-level type (WeixinMessage.MessageType).
const (
	MessageTypeUser = 1 // inbound: message from a WeChat user
	MessageTypeBot  = 2 // outbound: message from the bot
)

// Message-level state (WeixinMessage.MessageState).
const MessageStateFinish = 2

// Item-level type (MessageItem.Type) — mirrors MessageItemType in the iLink protocol.
const (
	MessageItemTypeText  = 1
	MessageItemTypeImage = 2
	MessageItemTypeVoice = 3
	MessageItemTypeFile  = 4
	MessageItemTypeVideo = 5
)

// channelVersion is sent in every request as base_info.channel_version so the
// server can identify the client. Matches the @tencent-weixin/openclaw-weixin
// plugin version we reverse-engineered the protocol from.
const channelVersion = "1.0.3"

// BaseInfo is attached to every outgoing API request body.
type BaseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

func buildBaseInfo() BaseInfo {
	return BaseInfo{ChannelVersion: channelVersion}
}

// TextItem holds the content of a text message.
type TextItem struct {
	Text string `json:"text"`
}

// CDNMedia is a CDN reference embedded in media items.
type CDNMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"` // base64-encoded; see parseAESKey for formats
	EncryptType       int    `json:"encrypt_type,omitempty"`
}

// ImageItem holds image metadata.
type ImageItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	ThumbMedia *CDNMedia `json:"thumb_media,omitempty"`
	// AESKey is a hex-encoded 16-byte key (preferred over Media.AESKey for inbound messages).
	AESKey string `json:"aeskey,omitempty"`
	URL    string `json:"url,omitempty"`
}

// VoiceItem holds voice/audio metadata.
type VoiceItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	EncodeType int       `json:"encode_type,omitempty"` // 6=silk (common)
	SampleRate int       `json:"sample_rate,omitempty"`
	Playtime   int       `json:"playtime,omitempty"` // milliseconds
	Text       string    `json:"text,omitempty"`     // voice-to-text transcript (if available)
}

// FileItem holds file attachment metadata.
type FileItem struct {
	Media    *CDNMedia `json:"media,omitempty"`
	FileName string    `json:"file_name,omitempty"`
	MD5      string    `json:"md5,omitempty"`
	Len      string    `json:"len,omitempty"` // file size as string
}

// VideoItem holds video metadata.
type VideoItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	VideoSize  int       `json:"video_size,omitempty"`
	PlayLength int       `json:"play_length,omitempty"`
}

// MessageItem is a single content item in a WeChat message.
type MessageItem struct {
	Type      int        `json:"type"`
	TextItem  *TextItem  `json:"text_item,omitempty"`
	ImageItem *ImageItem `json:"image_item,omitempty"`
	VoiceItem *VoiceItem `json:"voice_item,omitempty"`
	FileItem  *FileItem  `json:"file_item,omitempty"`
	VideoItem *VideoItem `json:"video_item,omitempty"`
}

// WeixinMessage is the core message type used for both inbound (getupdates)
// and outbound (sendmessage) calls.
type WeixinMessage struct {
	FromUserID   string        `json:"from_user_id"`           // "" for outbound bot messages
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`    // unique UUID per outbound message
	MessageType  int           `json:"message_type,omitempty"` // 1=USER (inbound), 2=BOT (outbound)
	MessageState int           `json:"message_state,omitempty"` // 2=FINISH for outbound
	ItemList     []MessageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

// GetUpdatesReq is the request body for ilink/bot/getupdates.
type GetUpdatesReq struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      BaseInfo `json:"base_info"`
}

// GetUpdatesResp is the response from ilink/bot/getupdates.
type GetUpdatesResp struct {
	Ret                  int             `json:"ret"`
	ErrCode              int             `json:"errcode"`
	ErrMsg               string          `json:"errmsg"`
	Msgs                 []WeixinMessage `json:"msgs"`
	GetUpdatesBuf        string          `json:"get_updates_buf"`
	LongpollingTimeoutMs int             `json:"longpolling_timeout_ms"` // server-suggested poll timeout
}

// ErrCodeSessionExpired is the iLink error code for an expired/invalidated
// session. When received, polling must stop and re-login is required.
const ErrCodeSessionExpired = -14

// SendMessageReq is the request body for ilink/bot/sendmessage.
type SendMessageReq struct {
	Msg      WeixinMessage `json:"msg"`
	BaseInfo BaseInfo      `json:"base_info"`
}

// SendMessageResp is the response from ilink/bot/sendmessage.
type SendMessageResp struct {
	Ret     int    `json:"ret"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// GetQRCodeResp is the response from ilink/bot/get_bot_qrcode.
type GetQRCodeResp struct {
	Ret           int    `json:"ret"`
	ErrMsg        string `json:"errmsg"`
	QRCode        string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// GetQRCodeStatusResp is the response from ilink/bot/get_qrcode_status.
type GetQRCodeStatusResp struct {
	Ret        int    `json:"ret"`
	ErrMsg     string `json:"errmsg"`
	Status     string `json:"status"` // "wait" | "scaned" | "confirmed" | "expired"
	BotToken   string `json:"bot_token"`
	ILinkBotID string `json:"ilink_bot_id"`
	BaseURL    string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}
