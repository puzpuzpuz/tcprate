# tcprate

Minimalistic rate limiter for TCP servers.

Aimed to follow the public contracts of `net.Listener` and `net.Conn` as much as possible.

## Usage

```go
lis, err := net.Listen("tcp", ":")
if err != nil {
    log.Fatal(err)
}
// Wrap the listener.
lim := tcprate.NewListener(lis)
// Set 64 KB/sec global limit and 4 KB/sec limit per connection.
lim.SetLimits(64*1024, 4*1024)
// Now lim can be used as any net.Listener.
// ...
```

## Known Limitations

* The rate limit is only applied to outbound server traffic.
* Write errors are ignored when calculating the rate limit.
* Both tests and functionality are minimal.

## License

Licensed under MIT.
