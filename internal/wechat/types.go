package wechat

// MessageTypeText is the iLink message type for plain text.
const MessageTypeText = 1

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
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
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
	Ret           int             `json:"ret"`
	ErrCode       int             `json:"errcode"`
	ErrMsg        string          `json:"errmsg"`
	Msgs          []WeixinMessage `json:"msgs"`
	GetUpdatesBuf string          `json:"get_updates_buf"`
}

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
