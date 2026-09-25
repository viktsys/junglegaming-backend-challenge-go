#!/bin/bash
# Provisioning executed by LocalStack on startup (ready.d hook).
# Creates the FIFO queues with their dead-letter queues and redrive policies.
# The application also provisions queues idempotently at boot, so this script
# exists to make the broker configuration explicit and inspectable.
set -euo pipefail

ACCOUNT=000000000000
REGION=us-east-1
ENDPOINT=http://localhost:4566

fifo_attributes() {
  echo "FifoQueue=true,VisibilityTimeout=30,MessageRetentionPeriod=1209600,ReceiveMessageWaitTimeSeconds=20"
}

create_dlq() {
  local name="$1"
  awslocal sqs create-queue \
    --queue-name "$name" \
    --attributes "$(fifo_attributes)"
  awslocal sqs get-queue-attributes \
    --queue-url "$ENDPOINT/$ACCOUNT/$name" \
    --attribute-names QueueArn \
    --query 'Attributes.QueueArn' \
    --output text
}

create_main() {
  local name="$1"
  local dlq_arn="$2"
  local redrive
  redrive=$(printf '{"deadLetterTargetArn":"%s","maxReceiveCount":"5"}' "$dlq_arn")
  awslocal sqs create-queue \
    --queue-name "$name" \
    --attributes "$(fifo_attributes),RedrivePolicy=$redrive"
}

TX_DLQ_ARN=$(create_dlq "wager-transactions-dlq.fifo")
EVENTS_DLQ_ARN=$(create_dlq "wager-events-dlq.fifo")

create_main "wager-transactions.fifo" "$TX_DLQ_ARN"
create_main "wager-events.fifo" "$EVENTS_DLQ_ARN"

echo "SQS queues provisioned:"
awslocal sqs list-queues --region "$REGION"
