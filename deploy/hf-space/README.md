---
title: tape
colorFrom: yellow
colorTo: gray
sdk: docker
app_port: 7860
pinned: false
short_description: Limit order book matching engine with a live simulated market
---

# tape

Backend for the tape demo: a price-time priority matching engine with a
write-ahead log, a REST and WebSocket gateway, and bot traders keeping four
markets busy.

- `GET /v1/symbols`, `GET /v1/book/{symbol}?depth=20`, `GET /v1/trades/{symbol}`, `GET /v1/stats`
- `POST /v1/orders`, `PATCH /v1/orders/{id}`, `DELETE /v1/orders/{id}` with an `X-Trader-ID` header (1000 or above)
- `GET /v1/ws/{symbol}`: L2 snapshot, then sequenced updates and trades

Source and documentation: see the project README.
