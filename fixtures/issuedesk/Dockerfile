FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY fixtures/issuedesk ./fixtures/issuedesk
RUN CGO_ENABLED=0 go build -trimpath -o /issuedesk ./fixtures/issuedesk/cmd/issuedesk
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /issuedesk /issuedesk
EXPOSE 8080
ENTRYPOINT ["/issuedesk"]
CMD ["-addr", "0.0.0.0:8080", "-seed"]
