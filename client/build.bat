set CGO_ENABLED=0
set GOOS=linux
go build -o proxy client.go