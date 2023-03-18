package bot

import (
	"context"
	"strings"
	"time"

	n "github.com/dyng/nosdaily/nostr"
	"github.com/dyng/nosdaily/service"
	"github.com/dyng/nosdaily/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/nbd-wtf/go-nostr"
	"github.com/robfig/cron/v3"
)

var logger = log.New("module", "bot")

type BotApplication struct {
	Bot    *Bot
	config *types.Config
	Worker *Worker
}

type Bot struct {
	client  *n.Client
	service *service.Service
	SK      string
	pub     string
}

func NewBotApplication(config *types.Config, service *service.Service) *BotApplication {
	ctx := context.Background()

	client, err := n.NewClient(ctx, config.Bot.Relays)
	if err != nil {
		panic(err)
	}

	bot, err := NewBot(ctx, client, service, config.Bot.SK)
	if err != nil {
		panic(err)
	}

	worker, err := NewWorker(ctx, client, service)
	if err != nil {
		panic(err)
	}

	return &BotApplication{
		Bot:    bot,
		config: config,
		Worker: worker,
	}
}

func (ba *BotApplication) Run(ctx context.Context) error {
	c, err := ba.Bot.Listen(ctx)
	if err != nil {
		logger.Crit("cannot listen to subscribe messages", "err", err)
	}

	logger.Info("start listening to subscribe messages...")

	done := make(chan struct{})
	defer close(done)

	go func(c <-chan nostr.Event) {
		for ev := range c {
			logger.Info("received mentioning event", "event", ev.Content)
			if strings.Contains(ev.Content, "#subscribe") {
				logger.Info("preparing channel", "pubkey", ev.PubKey)
				channelSK, new, err := ba.Bot.GetOrCreateSubscription(ctx, ev.PubKey)
				if err != nil {
					logger.Warn("failed to create channel", "pubkey", ev.PubKey, "err", err)
					continue
				}

				if new {
					ba.Bot.SendWelcomeMessage(ctx, channelSK, ev.PubKey)
					logger.Info("sent welcome message to new subscriber", "pubkey", ev.PubKey)
				} else {
					restored, err := ba.Bot.RestoreSubscription(ctx, ev.PubKey)
					if err != nil {
						logger.Warn("failed to restore subscription", "pubkey", ev.PubKey, "err", err)
					}

					if restored {
						logger.Info("sending welcome message to returning subscriber", "pubkey", ev.PubKey)
						err := ba.Bot.SendWelcomeMessage(ctx, channelSK, ev.PubKey)
						if err != nil {
							logger.Warn("failed to send welcome message returning subscriber", "pubkey", ev.PubKey, "err", err)
						}
					} else {
						logger.Info("skip welcome message for existing subscriber", "pubkey", ev.PubKey)
					}
				}
			} else if strings.Contains(ev.Content, "#unsubscribe") {
				logger.Warn("unsubscribing", "pubkey", ev.PubKey)
				ba.Bot.TerminateSubscription(ctx, ev.PubKey)
			}
		}

		done <- struct{}{}
	}(c)

	cr := cron.New()
	cr.AddFunc("0 * * * *", func() {
		logger.Info("running hourly cron job")
		ba.Worker.Batch(ctx, 100, 0) // TODO: should check next
	})

	<-done
	cr.Stop()
	logger.Info("bot exiting...")
	return nil
}

func NewBot(ctx context.Context, client *n.Client, service *service.Service, sk string) (*Bot, error) {
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		return nil, err
	}

	return &Bot{
		client:  client,
		SK:      sk,
		pub:     pub,
		service: service,
	}, nil
}

func (b *Bot) Listen(ctx context.Context) (<-chan nostr.Event, error) {
	now := time.Now()
	filters := nostr.Filters{
		nostr.Filter{
			Kinds: []int{1},
			Since: &now,
			Tags: nostr.TagMap{
				"p": []string{b.pub},
			},
		},
	}
	return b.client.Subscribe(ctx, filters), nil
}

func (b *Bot) GetOrCreateSubscription(ctx context.Context, subscriberPub string) (string, bool, error) {
	subscriber := b.service.GetSubscriber(subscriberPub)
	if subscriber != nil {
		logger.Info("found existing subscriber", "pubkey", subscriberPub)
		return subscriber.ChannelSecret, false, nil
	}

	logger.Info("creating new subscriber", "pubkey", subscriberPub)
	channelSK := nostr.GeneratePrivateKey()
	err := b.service.CreateSubscriber(subscriberPub, channelSK, time.Now())
	if err != nil {
		return "", false, err
	}

	return channelSK, true, nil
}

func (b *Bot) TerminateSubscription(ctx context.Context, subscriberPub string) error {
	return b.service.DeleteSubscriber(subscriberPub, time.Now())
}

func (b *Bot) RestoreSubscription(ctx context.Context, subscriberPub string) (bool, error) {
	return b.service.RestoreSubscriber(subscriberPub, time.Now())
}

func (b *Bot) SendWelcomeMessage(ctx context.Context, channelSK, receiverPub string) error {
	channelPub, err := nostr.GetPublicKey(channelSK)
	if err != nil {
		return err
	}

	msg := "Hello, #[0]! Your nossence recommendations is ready, follow: #[1] to fetch your own feed."
	return b.client.Mention(ctx, b.SK, msg, []string{
		receiverPub,
		channelPub,
	})
}
