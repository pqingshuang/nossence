package service

import (
	"context"
	"time"

	"github.com/dyng/nosdaily/database"
	"github.com/dyng/nosdaily/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/nbd-wtf/go-nostr"
	decodepay "github.com/nbd-wtf/ln-decodepay"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type Service struct {
	config *types.Config
	neo4j  *database.Neo4jDb
}

type IService interface {
	GetFeed(userPub string, start time.Time, end time.Time, limit int) []FeedEntry
}

func NewService(config *types.Config, neo4j *database.Neo4jDb) *Service {
	return &Service{
		config: config,
		neo4j:  neo4j,
	}
}

type FeedEntry struct {
	Id        string    `json:"event_id"`
	Kind      int       `json:"kind"`
	Pubkey    string    `json:"pubkey"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Score     int       `json:"score"`
}

func (s *Service) InitDatabase() error {
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()
		if _, err := tx.Run(ctx, "CREATE CONSTRAINT post_id_uniq IF NOT EXISTS FOR (p:Post) REQUIRE p.id IS UNIQUE;", nil); err != nil {
			return nil, err
		}
		if _, err := tx.Run(ctx, "CREATE CONSTRAINT user_pk_uniq IF NOT EXISTS FOR (u:User) REQUIRE u.pubkey IS UNIQUE;", nil); err != nil {
			return nil, err
		}
		return nil, nil
	})

	return err
}

func (s *Service) GetFeed(userPub string, start time.Time, end time.Time, limit int) []FeedEntry {
	posts, err := s.neo4j.ExecuteRead(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		result, err := tx.Run(ctx, "match (p:Post) where p.created_at > $Start and p.created_at < $End optional match (r1:Post)-[:REPLY]->(p) optional match (r2:Post)-[:LIKE]->(p) optional match (r3:Post)-[:ZAP]->(p) with p, count(distinct r1.author)*15+count(distinct r2.author)*10+count(distinct r3.author)*50 as score order by score desc limit $Limit return p.id, p.kind, p.author, p.content, p.created_at, score;",
			map[string]any{
				"Start": start.Unix(),
				"End":   end.Unix(),
				"Limit": limit,
			})

		if err != nil {
			return nil, err
		}

		posts := make([]FeedEntry, 0)
		for result.Next(ctx) {
			record := result.Record()
			post := FeedEntry{
				Id:        record.Values[0].(string),
				Kind:      int(record.Values[1].(int64)),
				Pubkey:    record.Values[2].(string),
				Content:   record.Values[3].(string),
				CreatedAt: time.Unix(record.Values[4].(int64), 0),
				Score:     int(record.Values[5].(int64)),
			}
			posts = append(posts, post)
		}
		return posts, nil
	})

	if err != nil {
		log.Error("Failed to get feed", "err", err)
		return nil
	} else {
		return posts.([]FeedEntry)
	}
}

func (s *Service) StoreEvent(event *nostr.Event) error {
	switch event.Kind {
	case 1:
		return s.StorePost(event)
	case 7:
		return s.StoreLike(event)
	case 3:
		return s.StoreContact(event)
	case 9735:
		return s.StoreZap(event)
	default:
		log.Warn("Unsupported event kind", "kind", event.Kind)
		return nil
	}
}

func (s *Service) StorePost(event *nostr.Event) error {
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		// create user & post
		if err := s.saveUserAndPost(ctx, tx, event); err != nil {
			return nil, err
		}

		// create reply relation
		refs := event.Tags.GetAll([]string{"e"})
		if len(refs) > 0 {
			ref := refs[0]
			if _, err := tx.Run(ctx, "match (p:Post), (r:Post) where p.id = $Id and r.id = $RefId merge (p)-[:REPLY]->(r);",
				map[string]any{
					"Id":    event.ID,
					"RefId": ref.Value(),
				}); err != nil {
				return nil, err
			}
		}

		return nil, nil
	})

	return err
}

func (s *Service) StoreLike(event *nostr.Event) error {
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		// create user & post
		if err := s.saveUserAndPost(ctx, tx, event); err != nil {
			return nil, err
		}

		// create like relation
		refs := event.Tags.GetAll([]string{"e"})
		if len(refs) > 0 {
			ref := refs[0]
			if _, err := tx.Run(ctx, "match (p:Post), (r:Post) where p.id = $Id and r.id = $RefId merge (p)-[:LIKE]->(r);",
				map[string]any{
					"Id":    event.ID,
					"RefId": ref.Value(),
				}); err != nil {
				return nil, err
			}
		}

		return nil, nil
	})

	return err
}

func (s *Service) StoreContact(event *nostr.Event) error {
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		// delete old follow relations
		if _, err := tx.Run(ctx, "match (u:User {pubkey: $Pubkey})-[r:FOLLOW]->() delete r;",
			map[string]any{
				"Pubkey": event.PubKey,
			}); err != nil {
			return nil, err
		}

		// create new follow relations
		tags := event.Tags.GetAll([]string{"p"})
		for _, pTag := range tags {
			if _, err := tx.Run(ctx, "merge (u:User {pubkey: $Pubkey}) merge (p:User {pubkey: $P}) merge (u)-[:FOLLOW]->(p);",
				map[string]any{
					"Pubkey": event.PubKey,
					"P":      pTag.Value(),
				}); err != nil {
				return nil, err
			}
		}

		return nil, nil
	})

	return err
}

func (s *Service) StoreZap(event *nostr.Event) error {
	// decode zap amount
	bolt11 := event.Tags.GetLast([]string{"bolt11"})
	invoice, err := decodepay.Decodepay(bolt11.Value())
	if err != nil {
		return err
	}
	amount := invoice.MSatoshi / 1000

	_, err = s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		// exit if not a zap to a post
		refs := event.Tags.GetAll([]string{"e"})
		if len(refs) == 0 {
			return nil, nil
		}

		// create user & post
		if err := s.saveUserAndPost(ctx, tx, event); err != nil {
			return nil, err
		}

		// create zap relation
		ref := refs[0]
		if _, err := tx.Run(ctx, "match (p:Post), (r:Post) where p.id = $Id and r.id = $RefId merge (p)-[:ZAP {amount: $Amount}]->(r);",
			map[string]any{
				"Id":     event.ID,
				"RefId":  ref.Value(),
				"Amount": amount,
			}); err != nil {
			return nil, err
		}

		return nil, nil
	})

	return err
}

func (s *Service) saveUserAndPost(ctx context.Context, tx neo4j.ManagedTransaction, event *nostr.Event) error {
	if _, err := tx.Run(ctx, "merge (u:User {pubkey: $Pubkey});",
		map[string]any{
			"Pubkey": event.PubKey,
		}); err != nil {
		return err
	}

	if _, err := tx.Run(ctx, "merge (p:Post {id: $Id, kind: $Kind, author: $Author, content: $Content, created_at: $CreatedAt});",
		map[string]any{
			"Id":        event.ID,
			"Kind":      event.Kind,
			"Author":    event.PubKey,
			"Content":   event.Content,
			"CreatedAt": event.CreatedAt.Unix(),
		}); err != nil {
		return err
	}

	if _, err := tx.Run(ctx, "match (u:User), (p:Post) where u.pubkey = $Pubkey and p.id = $Id merge (u)-[:CREATE]->(p);",
		map[string]any{
			"Pubkey": event.PubKey,
			"Id":     event.ID,
		}); err != nil {
		return err
	}

	return nil
}

func (s *Service) CreateSubscriber(pubkey, channelSK string, subscribedAt time.Time) error {
	log.Debug("Create subscriber", "pubkey", pubkey)
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(context.Background(), "MERGE (s:Subscriber {pubkey: $Pubkey}) ON CREATE SET s.channel_secret = $ChannelSecret, s.subscribed_at = $SubscribedAt, s.unsubscribed_at = null;",
			map[string]any{
				"Pubkey":        pubkey,
				"ChannelSecret": channelSK,
				"SubscribedAt":  subscribedAt.Unix(),
			})
		return nil, err
	})
	return err
}

func (s *Service) GetSubscriber(pubkey string) *types.Subscriber {
	subscriber, err := s.neo4j.ExecuteRead(func(tx neo4j.ManagedTransaction) (any, error) {
		ctx := context.Background()

		result, err := tx.Run(ctx, "MATCH (s:Subscriber {pubkey: $Pubkey}) RETURN s.pubkey, s.channel_secret, s.subscribed_at, s.unsubscribed_at;",
			map[string]any{
				"Pubkey": pubkey,
			})
		if err != nil {
			return nil, err
		}

		record, err := result.Single(ctx)
		if err != nil {
			return nil, err
		}

		subscriber := types.Subscriber{
			Pubkey:         record.Values[0].(string),
			ChannelSecret:  record.Values[1].(string),
			SubscribedAt:   time.Unix(record.Values[2].(int64), 0),
			UnsubscribedAt: time.Unix(record.Values[3].(int64), 0),
		}

		return subscriber, nil
	})

	if err != nil {
		log.Error("Failed to get subscriber", "err", err)
		return nil
	}

	if result, ok := subscriber.(types.Subscriber); ok {
		return &result
	}

	return nil
}

func (s *Service) DeleteSubscriber(pubkey string, unsubscribedAt time.Time) error {
	log.Debug("Deleting subscriber", "pubkey", pubkey)
	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(context.Background(), "MATCH (s:Subscriber {pubkey: $Pubkey}) SET s.unsubscribed_at = $UnsubscribedAt;",
			map[string]any{
				"Pubkey":         pubkey,
				"UnsubscribedAt": unsubscribedAt.Unix(),
			})
		return nil, err
	})
	return err
}

func (s *Service) RestoreSubscriber(pubkey string, subscribedAt time.Time) (bool, error) {
	log.Debug("Restore subscriber", "pubkey", pubkey)

	subscriber := s.GetSubscriber(pubkey)
	if subscriber.UnsubscribedAt.IsZero() {
		return false, nil
	}

	_, err := s.neo4j.ExecuteWrite(func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(context.Background(), "MATCH (s:Subscriber {pubkey: $Pubkey}) SET s.unsubscribed_at = null, s.subscribed_at = $SubscribedAt;",
			map[string]any{
				"Pubkey":       pubkey,
				"SubscribedAt": subscribedAt.Unix(),
			})

		return nil, err
	})

	if err != nil {
		return false, err
	}

	return true, err
}
