package sqs

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// Consumer polls the transactions queue and hands messages to a handler.
// Messages are deleted only after the handler confirms durable processing.
// On shutdown the in-flight message is completed within a grace period;
// otherwise its visibility expires and the broker redelivers it.
type Consumer struct {
	client        *Client
	handler       ports.MessageHandler
	logger        *slog.Logger
	metrics       ports.Metrics
	shutdownGrace time.Duration
	done          chan struct{}
}

// NewConsumer builds the consumer.
func NewConsumer(client *Client, handler ports.MessageHandler, metrics ports.Metrics, logger *slog.Logger, shutdownGrace time.Duration) *Consumer {
	if shutdownGrace <= 0 {
		shutdownGrace = 20 * time.Second
	}
	return &Consumer{
		client:        client,
		handler:       handler,
		logger:        logger,
		metrics:       metrics,
		shutdownGrace: shutdownGrace,
		done:          make(chan struct{}),
	}
}

// Run polls until the context is cancelled. It always closes Done.
func (c *Consumer) Run(ctx context.Context) {
	defer close(c.done)
	c.logger.Info("sqs consumer started",
		slog.String("queue", c.client.cfg.QueueName),
		slog.Int("maxMessages", int(c.client.cfg.MaxMessages)))

	backoff := time.Second
	for ctx.Err() == nil {
		messages, err := c.client.receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			c.logger.Error("sqs receive failed", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, message := range messages {
			c.processOne(ctx, message)
		}
	}
	c.logger.Info("sqs consumer stopped")
}

// Done is closed when Run returns.
func (c *Consumer) Done() <-chan struct{} { return c.done }

// Wait blocks until the consumer stopped or the deadline expired.
func (c *Consumer) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Consumer) processOne(ctx context.Context, message types.Message) {
	// Keep processing the in-flight message during shutdown, bounded by the
	// grace period, so a committed operation can still be acknowledged.
	processCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.shutdownGrace)
	defer cancel()

	received := ports.ReceivedMessage{
		MessageID:     valueOr(message.MessageId, ""),
		ReceiptHandle: valueOr(message.ReceiptHandle, ""),
		Body:          valueOr(message.Body, ""),
		ReceiveCount:  receiveCount(message),
	}

	logger := c.logger.With(
		slog.String("messageId", received.MessageID),
		slog.Int("receiveCount", received.ReceiveCount))

	err := c.handler.Handle(processCtx, received)
	switch {
	case err == nil:
		if deleteErr := c.client.deleteMessage(processCtx, c.client.transactionsQueueURL, received.ReceiptHandle); deleteErr != nil {
			logger.Error("failed to delete acknowledged message", slog.String("error", deleteErr.Error()))
			return
		}
		logger.Debug("message acknowledged")

	case ports.IsPermanent(err):
		logger.Warn("message is unprocessable; routing to dlq", slog.String("error", err.Error()))
		if dlqErr := c.client.sendToDLQ(processCtx, received.Body, received.MessageID); dlqErr != nil {
			logger.Error("failed to route message to dlq", slog.String("error", dlqErr.Error()))
			return
		}
		c.metrics.RecordDLQMessage(c.client.cfg.DLQName)
		if deleteErr := c.client.deleteMessage(processCtx, c.client.transactionsQueueURL, received.ReceiptHandle); deleteErr != nil {
			logger.Error("failed to delete dlq-routed message", slog.String("error", deleteErr.Error()))
		}

	default:
		// Transient failure: leave the message invisible until the visibility
		// timeout expires. The redrive policy moves it to the DLQ after
		// maxReceiveCount deliveries.
		logger.Warn("transient processing failure; message will be redelivered",
			slog.String("error", err.Error()))
	}
}

func receiveCount(message types.Message) int {
	raw, ok := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]
	if !ok {
		return 1
	}
	count, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	return count
}

func valueOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}
