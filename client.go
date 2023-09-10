package goxstreams

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient required to use cluster client
type RedisClient interface {
	redis.Cmdable
}

type streamClient struct {
	client        RedisClient
	groupReadArgs *redis.XReadGroupArgs
	pendingArgs   *redis.XPendingExtArgs
	claimArgs     *redis.XClaimArgs
}

type clientParams struct {
	Stream       string
	Group        string
	Consumer     string
	Batch        int64
	NoAck        bool
	Idle         time.Duration
	ReadInterval time.Duration
}

func newClient(client RedisClient, params clientParams) streamClient {
	groupReadArgs := &redis.XReadGroupArgs{
		Streams:  []string{params.Stream, ">"},
		Group:    params.Group,
		Consumer: params.Consumer,
		Count:    params.Batch,
		NoAck:    params.NoAck,
		Block:    params.ReadInterval,
	}

	pendingArgs := &redis.XPendingExtArgs{
		Stream:   params.Stream,
		Group:    params.Group,
		Consumer: params.Consumer,
		Count:    params.Batch,
		Start:    "-",
		End:      "+",
		Idle:     params.Idle,
	}

	claimArgs := &redis.XClaimArgs{
		Stream:   params.Stream,
		Group:    params.Group,
		Consumer: params.Consumer,
		MinIdle:  params.Idle,
	}

	streamClient := streamClient{
		client:        client,
		groupReadArgs: groupReadArgs,
		pendingArgs:   pendingArgs,
		claimArgs:     claimArgs,
	}

	return streamClient
}

func newClientWithGroupInit(ctx context.Context, client RedisClient, params clientParams) (streamClient, error) {
	return newClient(client, params).init(ctx, params)
}

func (c streamClient) init(ctx context.Context, params clientParams) (streamClient, error) {
	if params.Stream == "" || params.Group == "" {
		return c, errors.New("stream and group can not be empty")
	}

	infos, err := c.client.XInfoGroups(ctx, params.Stream).Result()
	if err != nil {
		if err.Error() == "ERR no such key" {
			if _, err = c.client.XGroupCreateMkStream(ctx, params.Stream, params.Group, "0").Result(); err != nil {
				return c, err
			}

			return c, nil
		}
		return c, err
	}

	for _, info := range infos {
		if info.Name == params.Group {
			return c, nil
		}
	}

	_, err = c.client.XGroupCreate(ctx, params.Stream, params.Group, "0").Result()
	return c, err
}

func (c streamClient) add(
	ctx context.Context, stream string, event map[string]interface{},
) error {
	_, err := c.client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: event}).Result()

	return err
}

func (c streamClient) addBatch(
	ctx context.Context, stream string, events []map[string]interface{},
) error {
	_, err := c.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, event := range events {
			pipe.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: event})
		}
		return nil
	})

	return err
}

func (c streamClient) readEvents(ctx context.Context) ([]xRawMessage, error) {
	streams, err := c.client.XReadGroup(ctx, c.groupReadArgs).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}

	result := make([]xRawMessage, 0, len(streams[0].Messages))
	for _, message := range streams[0].Messages {
		result = append(result, newXMessage(message.ID, 0, message.Values))
	}

	return result, nil
}

func (c streamClient) readFailEvents(ctx context.Context) ([]xRawMessage, error) {
	pendingsIds, pendingCountByID, pendingErr := c.pending(ctx)
	if pendingErr != nil {
		return nil, pendingErr
	}

	if len(pendingsIds) == 0 {
		return nil, nil
	}

	events, eventsErr := c.claiming(ctx, pendingsIds)
	if eventsErr != nil {
		return nil, eventsErr
	}

	result := make([]xRawMessage, 0, len(pendingsIds))
	for _, event := range events {
		result = append(result, newXMessage(event.ID, pendingCountByID[event.ID], event.Values))
	}

	return result, nil
}

func (c streamClient) ack(ctx context.Context, ids []string) error {
	res := c.client.XAck(ctx, c.groupReadArgs.Streams[0], c.groupReadArgs.Group, ids...)

	return res.Err()
}

func (c streamClient) del(ctx context.Context, ids []string) error {
	res := c.client.XDel(ctx, c.groupReadArgs.Streams[0], ids...)

	return res.Err()
}

func (c streamClient) pending(ctx context.Context) ([]string, map[string]int64, error) {
	pendings, err := c.client.XPendingExt(ctx, c.pendingArgs).Result()
	if err != nil {
		return nil, nil, err
	}

	if len(pendings) == 0 {
		return nil, nil, nil
	}

	pendingIds := make([]string, 0, len(pendings))
	pendingCountByID := make(map[string]int64, len(pendings))
	for _, pendingEvent := range pendings {
		pendingIds = append(pendingIds, pendingEvent.ID)
		pendingCountByID[pendingEvent.ID] = pendingEvent.RetryCount
	}
	return pendingIds, pendingCountByID, nil
}

func (c streamClient) claiming(ctx context.Context, ids []string) ([]redis.XMessage, error) {
	if len(ids) > 0 {
		events, err := c.claim(ctx, ids)
		if err != nil {
			return events, err
		}

		return events, nil
	}

	return nil, nil
}

func (c streamClient) claim(ctx context.Context, ids []string) ([]redis.XMessage, error) {
	args := c.claimArgs
	args.Messages = ids
	rawEvents, err := c.client.XClaim(ctx, args).Result()

	return rawEvents, err
}
