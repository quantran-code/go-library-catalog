package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The broker topology of the import jobs. Every name is fixed by the job
// contract, so a consumer can be pointed at the queues by name.
const (
	// jobsExchange carries the batch messages. It is direct and durable.
	jobsExchange = "library.jobs"
	// batchQueueName is the durable queue the consumer reads batches from.
	batchQueueName = "books.import.batch"
	// batchRoutingKey is the routing key of a batch message.
	batchRoutingKey = "books.import.batch"
	// deadLetterExchange carries the batches that exhausted their retries.
	deadLetterExchange = "library.jobs.dlq"
	// deadLetterQueueName is the queue that keeps those dead batches.
	deadLetterQueueName = "books.import.batch.dead"
	// deadLetterRoutingKey is the routing key of a dead-lettered batch.
	deadLetterRoutingKey = "books.import.batch.dead"
	// attemptsHeader counts the retries a delivery has already used, so an
	// absent header means the first delivery and the value is incremented by
	// one on every republish.
	attemptsHeader = "x-attempts"
	// batchContentType is the content type of a batch message body.
	batchContentType = "application/json"
)

// retryQueue is one fixed-delay retry queue. A message parked on it stays for
// TTL and is then dead-lettered back to jobsExchange with batchRoutingKey, so it
// returns to the batch queue to be handled again.
type retryQueue struct {
	Name string
	TTL  time.Duration
}

// retryQueues are the three fixed delays in the order they are used: the first
// failure parks the batch for a second, the second for five, the third for
// fifteen.
var retryQueues = []retryQueue{
	{Name: "books.import.batch.retry.1s", TTL: time.Second},
	{Name: "books.import.batch.retry.5s", TTL: 5 * time.Second},
	{Name: "books.import.batch.retry.15s", TTL: 15 * time.Second},
}

// nextDisposition names where a failed delivery goes next. retriesUsed is the
// number of retries the delivery has already used, so the value 0 is the first
// failure. While retries remain the delivery moves to the retry queue for that
// retry; once the budget is spent it goes to the dead-letter queue and
// deadLettered is true. It is a pure function so the ordering is testable
// without a broker.
func nextDisposition(retriesUsed, maxAttempts int) (queue string, deadLettered bool) {
	if retriesUsed < 0 {
		retriesUsed = 0
	}
	if retriesUsed < maxAttempts && retriesUsed < len(retryQueues) {
		return retryQueues[retriesUsed].Name, false
	}
	return deadLetterQueueName, true
}

// retryQueueArgs is the argument table of one retry queue: the queue-level delay
// and the queue-level dead-lettering back to the batch queue.
func retryQueueArgs(queue retryQueue) amqp.Table {
	return amqp.Table{
		"x-message-ttl":             int32(queue.TTL.Milliseconds()),
		"x-dead-letter-exchange":    jobsExchange,
		"x-dead-letter-routing-key": batchRoutingKey,
	}
}

// declareJobTopology declares the exchange, the batch queue and its binding, the
// dead-letter exchange and queue, and the three retry queues. Every declaration
// is durable, so a broker restart keeps the jobs still in flight.
func declareJobTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(jobsExchange, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declaring exchange %s: %w", jobsExchange, err)
	}
	if _, err := channel.QueueDeclare(batchQueueName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declaring queue %s: %w", batchQueueName, err)
	}
	if err := channel.QueueBind(batchQueueName, batchRoutingKey, jobsExchange, false, nil); err != nil {
		return fmt.Errorf("binding queue %s: %w", batchQueueName, err)
	}

	if err := channel.ExchangeDeclare(deadLetterExchange, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declaring exchange %s: %w", deadLetterExchange, err)
	}
	if _, err := channel.QueueDeclare(deadLetterQueueName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declaring queue %s: %w", deadLetterQueueName, err)
	}
	if err := channel.QueueBind(deadLetterQueueName, deadLetterRoutingKey, deadLetterExchange, false, nil); err != nil {
		return fmt.Errorf("binding queue %s: %w", deadLetterQueueName, err)
	}

	for _, queue := range retryQueues {
		if _, err := channel.QueueDeclare(queue.Name, true, false, false, false, retryQueueArgs(queue)); err != nil {
			return fmt.Errorf("declaring queue %s: %w", queue.Name, err)
		}
	}
	return nil
}

// amqpPublisher publishes batch messages to the job exchange.
type amqpPublisher struct {
	conn    *amqp.Connection
	channel *amqp.Channel
}

// newAMQPPublisher connects to the broker and declares the job topology. The
// dial error is returned unchanged so the caller can decide to carry on with job
// submission disabled.
func newAMQPPublisher(brokerURL string) (*amqpPublisher, error) {
	conn, err := amqp.Dial(brokerURL)
	if err != nil {
		return nil, err
	}

	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening publisher channel: %w", err)
	}

	if err := declareJobTopology(channel); err != nil {
		_ = channel.Close()
		_ = conn.Close()
		return nil, err
	}
	return &amqpPublisher{conn: conn, channel: channel}, nil
}

// PublishBatch publishes one batch as a persistent message whose message id is
// the job id and batch number, so a redelivery can be recognised.
func (p *amqpPublisher) PublishBatch(ctx context.Context, msg batchMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding batch %d of job %s: %w", msg.Batch, msg.JobID, err)
	}

	err = p.channel.PublishWithContext(ctx, jobsExchange, batchRoutingKey, false, false, amqp.Publishing{
		ContentType:  batchContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    msg.JobID + ":" + strconv.Itoa(msg.Batch),
		Body:         body,
	})
	if err != nil {
		return fmt.Errorf("publishing batch %d of job %s: %w", msg.Batch, msg.JobID, err)
	}
	return nil
}

// Close releases the publisher channel and its connection.
func (p *amqpPublisher) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// amqpBatchConsumer reads batch messages and settles each one by hand: the
// delivery is acknowledged only once handle succeeded, or once the message was
// safely parked on a retry queue or the dead-letter queue.
type amqpBatchConsumer struct {
	conn         *amqp.Connection
	channel      *amqp.Channel
	handle       func(context.Context, batchMessage) error
	onDeadLetter func(batchMessage)
	maxAttempts  int
}

// newAMQPBatchConsumer connects to the broker, declares the job topology and
// asks for one unacknowledged delivery at a time.
func newAMQPBatchConsumer(brokerURL string, handle func(context.Context, batchMessage) error, onDeadLetter func(batchMessage), maxAttempts int) (*amqpBatchConsumer, error) {
	conn, err := amqp.Dial(brokerURL)
	if err != nil {
		return nil, err
	}

	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening consumer channel: %w", err)
	}

	if err := declareJobTopology(channel); err != nil {
		_ = channel.Close()
		_ = conn.Close()
		return nil, err
	}

	if err := channel.Qos(1, 0, false); err != nil {
		_ = channel.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("setting prefetch to 1: %w", err)
	}

	return &amqpBatchConsumer{
		conn:         conn,
		channel:      channel,
		handle:       handle,
		onDeadLetter: onDeadLetter,
		maxAttempts:  maxAttempts,
	}, nil
}

// Consume reads batches until ctx is cancelled or the delivery channel closes.
// A cancelled context is a clean stop and answers nil.
func (c *amqpBatchConsumer) Consume(ctx context.Context) error {
	deliveries, err := c.channel.Consume(batchQueueName, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consuming %s: %w", batchQueueName, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel of %s closed", batchQueueName)
			}
			c.handleDelivery(ctx, delivery)
		}
	}
}

// Close releases the consumer channel and its connection.
func (c *amqpBatchConsumer) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// handleDelivery runs one delivery through the batch handler and settles it. A
// successful batch is acknowledged. A failed one is republished to the retry
// queue or the dead-letter queue its retry budget names; the original delivery
// is acknowledged only after that republish succeeded, and a failed republish
// nacks it with requeue so the message is not dropped.
func (c *amqpBatchConsumer) handleDelivery(ctx context.Context, delivery amqp.Delivery) {
	var msg batchMessage
	if err := json.Unmarshal(delivery.Body, &msg); err != nil {
		// A body that is not a batch can never succeed, so it goes straight to
		// the dead-letter queue without naming a job that could be failed.
		log.Printf("catalog import job: message %q is not a batch: %v", delivery.MessageId, err)
		if err := c.republish(ctx, deadLetterExchange, deadLetterRoutingKey, delivery.Body, attemptsUsed(delivery)+1, delivery.MessageId); err != nil {
			log.Printf("catalog import job: dead-lettering message %q: %v", delivery.MessageId, err)
			_ = delivery.Nack(false, true)
			return
		}
		_ = delivery.Ack(false)
		return
	}

	if err := c.handle(ctx, msg); err != nil {
		retriesUsed := attemptsUsed(delivery)
		queue, deadLettered := nextDisposition(retriesUsed, c.maxAttempts)
		log.Printf("catalog import job %s batch %d failed after %d retries: %v", msg.JobID, msg.Batch, retriesUsed, err)

		exchange, routingKey := "", queue
		if deadLettered {
			exchange, routingKey = deadLetterExchange, deadLetterRoutingKey
		}
		if err := c.republish(ctx, exchange, routingKey, delivery.Body, retriesUsed+1, delivery.MessageId); err != nil {
			log.Printf("catalog import job %s batch %d: republish to %s: %v", msg.JobID, msg.Batch, queue, err)
			_ = delivery.Nack(false, true)
			return
		}

		// The job is failed exactly once the dead batch reached its queue.
		if deadLettered && c.onDeadLetter != nil {
			c.onDeadLetter(msg)
		}
		_ = delivery.Ack(false)
		return
	}

	_ = delivery.Ack(false)
}

// republish hands body to exchange and routingKey with the retries already used
// written into the attempts header, so a message that keeps failing walks the
// retry queues in order and is dead-lettered once the budget is spent.
func (c *amqpBatchConsumer) republish(ctx context.Context, exchange, routingKey string, body []byte, retriesUsed int, messageID string) error {
	return c.channel.PublishWithContext(ctx, exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  batchContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    messageID,
		Headers:      amqp.Table{attemptsHeader: int32(retriesUsed)},
		Body:         body,
	})
}

// attemptsUsed reads how many retries a delivery has already used. The header is
// absent on the first delivery.
func attemptsUsed(delivery amqp.Delivery) int {
	raw, ok := delivery.Headers[attemptsHeader]
	if !ok {
		return 0
	}
	switch value := raw.(type) {
	case int32:
		return int(value)
	case int64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}
