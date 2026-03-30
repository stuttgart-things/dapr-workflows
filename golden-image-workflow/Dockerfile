FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o golden-image-workflow .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/golden-image-workflow /
USER nonroot
CMD ["/golden-image-workflow"]
