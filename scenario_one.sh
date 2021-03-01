#!/usr/bin/env bash
set -Eeuxo pipefail

TOKEN1=$(curl --no-progress-meter http://localhost:8000/register/A)
TOKEN2=$(curl --no-progress-meter http://localhost:8000/register/B)
TOKEN3=$(curl --no-progress-meter http://localhost:8000/register/C)
TOKEN4=$(curl --no-progress-meter http://localhost:8000/register/D)

curl -i -H "Token: ${TOKEN1}" http://localhost:8000/balance -d '{"topup_amount": 1, "currency": "BTC"}'
curl -i -H "Token: ${TOKEN2}" http://localhost:8000/balance -d '{"topup_amount": 10, "currency": "BTC"}'
curl -i -H "Token: ${TOKEN3}" http://localhost:8000/balance -d '{"topup_amount": 250000, "currency": "USD"}'
curl -i -H "Token: ${TOKEN4}" http://localhost:8000/balance -d '{"topup_amount": 300000, "currency": "USD"}'

# The locally generated webhook URLs are unusable in practice
# because webhook.site does not allow anonymous users
# to look at payloads delivered within the requests with arbitrary URL paths.

# this standing order will immediately be cancelled because user A does not have enough BTC to fully satisfy it
curl -i -H "Token: ${TOKEN1}" http://localhost:8000/standing_order -d '{"type": "SELL", "limit_price": 10000, "quantity": 10, "webhook_url": "https://webhook.site/'"$TOKEN1"'"}'
# top up the balance once more
curl -i -H "Token: ${TOKEN1}" http://localhost:8000/balance -d '{"topup_amount": 9, "currency": "BTC"}'
# creation of this standing order now succeeds
curl -i -H "Token: ${TOKEN1}" http://localhost:8000/standing_order -d '{"type": "SELL", "limit_price": 10000, "quantity": 10, "webhook_url": "https://webhook.site/'"$TOKEN1"'"}'

curl -i -H "Token: ${TOKEN2}" http://localhost:8000/standing_order -d '{"type": "SELL", "limit_price": 20000, "quantity": 10, "webhook_url": "https://webhook.site/'"$TOKEN2"'"}'

curl -i -H "Token: ${TOKEN3}" http://localhost:8000/market_order -d '{"type": "BUY", "quantity": 15}'

ORDER_ID=$(curl --no-progress-meter -H "Token: ${TOKEN4}" http://localhost:8000/standing_order -d '{"type": "BUY", "limit_price": 10000, "quantity": 20, "webhook_url": "https://webhook.site/'"$TOKEN4"'"}')
ORDER_ID=${ORDER_ID#'{"ID":'}
ORDER_ID=${ORDER_ID%'}'}
# this standing order will immediately be cancelled because user D does not have enough BTC to fully satisfy it together with its previous order with ID ${ORDER_ID}
curl -i -H "Token: ${TOKEN4}" http://localhost:8000/standing_order -d '{"type": "BUY", "limit_price": 25000, "quantity": 10, "webhook_url": "https://webhook.site/'"$TOKEN4"'"}'
curl -i -H "Token: ${TOKEN4}" -X DELETE http://localhost:8000/standing_order/${ORDER_ID}
curl -i -H "Token: ${TOKEN4}" http://localhost:8000/standing_order -d '{"type": "BUY", "limit_price": 25000, "quantity": 10, "webhook_url": "https://webhook.site/'"$TOKEN4"'"}'

# Expected final state: All standing orders except for the last one are either cancelled or fulfilled. The last order is half fulfilled.
