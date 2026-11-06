package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"larkpan/internal/sign"
)

// defaultTTL is the lifetime of bot-issued lp_ tokens (30 days).
const defaultTTL = 30 * 24 * 3600

// Bot issues lp_ tokens to users who DM it. It runs on the SDK's
// WebSocket long connection, so the server needs no public callback URL.
// Only tenant members can reach a custom app's bot — that is the auth
// boundary; allow further restricts it to specific open_ids.
type Bot struct {
	appID, appSecret string
	domain           string
	lark             *lark.Client // for message replies
	secret           string       // lp_ signing secret
	allow            map[string]bool
	store            *LoginStore
}

// New builds a Bot. allow == nil permits any tenant member who can reach
// the bot; otherwise only the listed open_ids may be issued tokens.
func New(appID, appSecret, domain string, larkClient *lark.Client, secret string, allow []string, store *LoginStore) *Bot {
	b := &Bot{
		appID: appID, appSecret: appSecret, domain: domain,
		lark: larkClient, secret: secret, store: store,
	}
	if len(allow) > 0 {
		m := make(map[string]bool, len(allow))
		for _, id := range allow {
			if id = strings.TrimSpace(id); id != "" {
				m[id] = true
			}
		}
		if len(m) > 0 {
			b.allow = m
		}
	}
	return b
}

// Run connects the long-connection event stream; blocks until ctx ends
// or the connection fails (the SDK auto-reconnects internally).
func (b *Bot) Run(ctx context.Context) error {
	disp := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(b.onMessage)
	cli := larkws.NewClient(b.appID, b.appSecret,
		larkws.WithEventHandler(disp),
		larkws.WithDomain(b.domain),
		larkws.WithLogLevel(larkcore.LogLevelWarn))
	log.Printf("bot: connecting long-connection event stream")
	return cli.Start(ctx)
}

func (b *Bot) onMessage(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
	msg, sender := ev.Event.Message, ev.Event.Sender
	if msg == nil || sender == nil || sender.SenderId == nil {
		return nil
	}
	if larkcore.StringValue(sender.SenderType) != "user" {
		return nil // ignore bots (incl. ourselves) to avoid loops
	}
	openID := larkcore.StringValue(sender.SenderId.OpenId)
	if openID == "" {
		return nil
	}
	if larkcore.StringValue(msg.MessageType) != "text" {
		return b.reply(ctx, msg, "只懂文字消息，发 `help` 看用法")
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(larkcore.StringValue(msg.Content)), &body); err != nil {
		return nil
	}
	args := strings.Fields(strings.TrimSpace(body.Text))
	cmd := "help"
	if len(args) > 0 {
		cmd = strings.ToLower(args[0])
	}
	switch cmd {
	case "whoami":
		return b.reply(ctx, msg, "你的 open_id: "+openID)
	case "token":
		if !b.allowed(openID) {
			return b.reply(ctx, msg, "抱歉，你不在签发白名单里")
		}
		label := openID
		if len(args) > 1 {
			label = args[1] + "-" + openID
		}
		exp := time.Now().Unix() + defaultTTL
		tok := sign.ClientToken(b.secret, label, exp)
		log.Printf("bot: issued token label=%q for %s", label, openID)
		return b.reply(ctx, msg, fmt.Sprintf("token (30 天, 到期 %s):\n%s\n\nlabel=%s", time.Unix(exp, 0).Format("2006-01-02"), tok, label))
	case "login":
		if len(args) < 2 {
			return b.reply(ctx, msg, "用法: login <配对码>  (终端里 larkpan client login 会给)")
		}
		if !b.allowed(openID) {
			return b.reply(ctx, msg, "抱歉，你不在签发白名单里")
		}
		code := strings.ToUpper(args[1])
		if _, ok := b.store.Get(code); !ok {
			return b.reply(ctx, msg, "配对码无效或已过期——回终端重新跑 larkpan client login")
		}
		exp := time.Now().Unix() + defaultTTL
		tok := sign.ClientToken(b.secret, openID, exp)
		b.store.Issue(code, tok, openID, exp)
		log.Printf("bot: paired login code=%s label=%q", code, openID)
		return b.reply(ctx, msg, "已签发，回终端继续")
	default:
		return b.reply(ctx, msg, strings.Join([]string{
			"LarkPan 机器人:",
			"  token [名字]  给自己签一枚 30 天的访问 token",
			"  login <配对码>  完成终端登录",
			"  whoami          查看你的 open_id",
			"  help            这段说明",
		}, "\n"))
	}
}

func (b *Bot) allowed(openID string) bool {
	return len(b.allow) == 0 || b.allow[openID]
}

func (b *Bot) reply(ctx context.Context, msg *larkim.EventMessage, text string) error {
	if b.lark == nil {
		return nil
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	resp, err := b.lark.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(larkcore.StringValue(msg.MessageId)).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		log.Printf("bot: reply failed: %v", err)
		return err
	}
	if !resp.Success() {
		log.Printf("bot: reply api error: %s", resp.CodeError.String())
		return fmt.Errorf("reply: %s", resp.CodeError.String())
	}
	return nil
}
