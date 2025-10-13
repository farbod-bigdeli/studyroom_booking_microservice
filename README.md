# StudyRoom – Microservices (gRPC Architecture)

A **microservice** version of StudyRoom using:
- API Gateway (HTTP)
- Auth Service
- Rooms Service (HTTP + gRPC)
- Bookings Service (HTTP + gRPC)
- MongoDB + Mongo Express

Public users talk to the gateway via HTTP; internal services use **gRPC**.

---

##  Features
- Same as monolith: auth, admin room/schedule, search, booking, waitlist
- Inter-service communication via **gRPC**
- Each service containerized separately
- Measured with `wrk` for throughput/latency

---

##  Prerequisites
- Docker & Docker Compose
- `curl`, `jq`
- [`wrk`](https://github.com/wg/wrk) (or dockerized `williamyeh/wrk`)
- `protoc` only if editing `.proto` files

---

##  Run the Stack

```bash
docker compose up -d --build
docker compose ps
