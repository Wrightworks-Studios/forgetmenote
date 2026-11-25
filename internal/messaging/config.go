package messaging

type SlackConfig struct {
	Members           *MemberAccess        `json:"members,omitempty"`
	KnowledgeChannels map[string]string    `json:"knowledgeChannels,omitempty"`
	Broadcasts        map[string]Broadcast `json:"broadcasts,omitempty"`
	Team              string               `json:"team"`
	SigningSecretFile string               `json:"signingSecretFile"`
	BotTokenFile      string               `json:"botTokenFile"`
	Users             map[string]string    `json:"users"`
	Channels          map[string]Binding   `json:"channels"`
	DirectMessages    map[string]Binding   `json:"directMessages,omitempty"`
}
