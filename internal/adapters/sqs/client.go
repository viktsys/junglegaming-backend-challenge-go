// Package sqs adapts AWS SQS (LocalStack/MiniStack locally) to the application
// ports: queue provisioning, a graceful consumer and an outbox event publisher.
package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// Client owns the SQS API and the resolved queue URLs.
type Client struct {
	api *sqs.Client
	cfg config.SQSConfig
	log *slog.Logger

	transactionsQueueURL string
	transactionsDLQURL   string
	eventsQueueURL       string
	eventsDLQURL         string
}

// NewClient builds the SQS client. When an endpoint override is configured the
// client targets LocalStack/MiniStack.
func NewClient(ctx context.Context, cfg config.SQSConfig, logger *slog.Logger) (*Client, error) {
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("sqs: load aws config: %w", err)
	}

	api := sqs.NewFromConfig(awsCfg, func(options *sqs.Options) {
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})

	client := &Client{api: api, cfg: cfg, log: logger}

	// The broker may still be starting (LocalStack/MiniStack). Retry queue
	// provisioning for a bounded period before giving up.
	deadline := time.Now().Add(60 * time.Second)
	for {
		err = client.EnsureQueues(ctx)
		if err == nil {
			return client, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		logger.Warn("sqs not ready; retrying", slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// EnsureQueues creates the four FIFO queues (two main, two dead-letter) with
// redrive policies when they do not exist yet. It is safe to call concurrently.
func (c *Client) EnsureQueues(ctx context.Context) error {
	transactionsDLQ, err := c.ensureQueue(ctx, c.cfg.DLQName, nil)
	if err != nil {
		return err
	}
	eventsDLQ, err := c.ensureQueue(ctx, c.cfg.OutboxDLQName, nil)
	if err != nil {
		return err
	}

	transactions, err := c.ensureQueue(ctx, c.cfg.QueueName, c.redrivePolicy(transactionsDLQ.arn))
	if err != nil {
		return err
	}
	events, err := c.ensureQueue(ctx, c.cfg.OutboxQueueName, c.redrivePolicy(eventsDLQ.arn))
	if err != nil {
		return err
	}

	c.transactionsQueueURL = transactions.url
	c.transactionsDLQURL = transactionsDLQ.url
	c.eventsQueueURL = events.url
	c.eventsDLQURL = eventsDLQ.url

	c.log.Info("sqs queues ready",
		slog.String("transactionsQueue", c.cfg.QueueName),
		slog.String("transactionsDLQ", c.cfg.DLQName),
		slog.String("eventsQueue", c.cfg.OutboxQueueName),
		slog.String("eventsDLQ", c.cfg.OutboxDLQName))
	return nil
}

type queueInfo struct {
	url string
	arn string
}

func (c *Client) ensureQueue(ctx context.Context, name string, redrive map[string]string) (queueInfo, error) {
	attributes := map[string]string{
		"FifoQueue":                     "true",
		"VisibilityTimeout":             strconv.Itoa(int(c.cfg.VisibilityTimeout.Seconds())),
		"MessageRetentionPeriod":        "1209600",
		"ReceiveMessageWaitTimeSeconds": strconv.Itoa(int(c.cfg.WaitTime.Seconds())),
	}
	if redrive != nil {
		policy, err := json.Marshal(redrive)
		if err != nil {
			return queueInfo{}, fmt.Errorf("sqs: encode redrive policy: %w", err)
		}
		attributes["RedrivePolicy"] = string(policy)
	}

	out, err := c.api.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: attributes,
	})
	if err != nil {
		// A concurrent creator may have won; fall back to lookup.
		url, lookupErr := c.queueURL(ctx, name)
		if lookupErr != nil {
			return queueInfo{}, fmt.Errorf("sqs: create queue %s: %w", name, err)
		}
		return c.queueInfo(ctx, url)
	}
	return c.queueInfo(ctx, aws.ToString(out.QueueUrl))
}

func (c *Client) queueURL(ctx context.Context, name string) (string, error) {
	out, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.QueueUrl), nil
}

func (c *Client) queueInfo(ctx context.Context, url string) (queueInfo, error) {
	out, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return queueInfo{}, fmt.Errorf("sqs: get queue attributes: %w", err)
	}
	return queueInfo{url: url, arn: out.Attributes[string(types.QueueAttributeNameQueueArn)]}, nil
}

func (c *Client) redrivePolicy(dlqARN string) map[string]string {
	return map[string]string{
		"deadLetterTargetArn": dlqARN,
		"maxReceiveCount":     strconv.Itoa(int(c.cfg.MaxReceiveCount)),
	}
}

// TransactionsQueueURL exposes the resolved queue URL.
func (c *Client) TransactionsQueueURL() string { return c.transactionsQueueURL }

// TransactionsDLQURL exposes the resolved dead-letter queue URL.
func (c *Client) TransactionsDLQURL() string { return c.transactionsDLQURL }

// EventsQueueURL exposes the resolved outbox events queue URL.
func (c *Client) EventsQueueURL() string { return c.eventsQueueURL }

// EventsDLQURL exposes the resolved outbox events dead-letter queue URL.
func (c *Client) EventsDLQURL() string { return c.eventsDLQURL }

// Ping verifies the broker is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(c.transactionsQueueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("sqs: ping: %w", err)
	}
	return nil
}

// Publish implements ports.EventPublisher. MessageGroupId keeps per-wallet
// ordering; MessageDeduplicationId preserves the stable eventId.
func (c *Client) Publish(ctx context.Context, event ports.OutboxEvent) error {
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.eventsQueueURL),
		MessageBody:            aws.String(string(event.Payload)),
		MessageGroupId:         aws.String(event.AggregateID.String()),
		MessageDeduplicationId: aws.String(event.EventID.String()),
	})
	if err != nil {
		return fmt.Errorf("sqs: publish event %s: %w", event.EventID, err)
	}
	return nil
}

func (c *Client) sendToDLQ(ctx context.Context, body, messageID string) error {
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.transactionsDLQURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String("dlq"),
		MessageDeduplicationId: aws.String(fmt.Sprintf("dlq-%s-%d", messageID, time.Now().UnixNano())),
	})
	if err != nil {
		return fmt.Errorf("sqs: send message %s to dlq: %w", messageID, err)
	}
	return nil
}

func (c *Client) deleteMessage(ctx context.Context, queueURL, receiptHandle string) error {
	_, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	return err
}

func (c *Client) receive(ctx context.Context) ([]types.Message, error) {
	out, err := c.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(c.transactionsQueueURL),
		MaxNumberOfMessages:         c.cfg.MaxMessages,
		WaitTimeSeconds:             int32(c.cfg.WaitTime.Seconds()),
		VisibilityTimeout:           int32(c.cfg.VisibilityTimeout.Seconds()),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, fmt.Errorf("sqs: receive messages: %w", err)
	}
	return out.Messages, nil
}
