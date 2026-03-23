package wechat

// Message-level type (WeixinMessage.MessageType).
const (
	MessageTypeUser = 1 // inbound: message from a WeChat user
	MessageTypeBot  = 2 // outbound: message from the bot
)

// Message-level state (WeixinMessage.MessageState).
const MessageStateFinish = 2

// Item-level type (MessageItem.Type).
const (
	MessageItemTypeText  = 1 // plain text
	MessageItemTypeImage = 2 // image
	MessageItemTypeVoice = 3 // voice/audio
	MessageItemTypeVideo = 4 // video
	MessageItemTypeFile  = 6 // generic file/document
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

// MessageItem is a single content item in a WeChat message.
type MessageItem struct {
	Type     int       `json:"type"`
	TextItem *TextItem `json:"text_item,omitempty"`
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
