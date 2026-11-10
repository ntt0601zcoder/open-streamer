# HTTP-TS không có timeout cho dial và TLS handshake
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor
- **Vị trí:** internal/ingestor/pull/httpts.go:39-42, 55-71, 86; internal/ingestor/worker.go:155-165

## Hiện tượng
Input `http(s)://...ts` trỏ tới host không trả lời TLS, hoặc IP bị blackhole, giữ worker trong `Open` rất lâu. Không có error history và không có retry trong suốt thời gian đó; chỉ có packet-timeout từ manager báo hiệu.

## Điều kiện kích hoạt
Host nhận TCP nhưng không hoàn tất TLS: `Open` chờ không giới hạn. Host không phản hồi SYN: chờ timeout TCP của hệ thống, trên Linux mặc định cỡ 2 phút.

## Phân tích
```go
transport := &http.Transport{
    DisableKeepAlives:     true,
    DisableCompression:    true,
    ResponseHeaderTimeout: timeoutOr(input.Net.TimeoutSec, httpTSDefaultDialTimeout),
}
```
`DialContext` là nil nên dùng dialer không có `Timeout`; `TLSHandshakeTimeout` bằng 0 nghĩa là không giới hạn. `ResponseHeaderTimeout` chỉ tính sau khi ghi xong request.

`openSource` chỉ gọi `handleOpenFailure` khi `Open` trả về, nên `onInputError` không chạy.

## Vì sao đây là lỗi
Comment của const `httpTSDefaultDialTimeout` nói "caps the initial HTTP connect + headers exchange", và comment ở client nói "Per-request dial / TLS / headers timeout". Chỉ phần headers được giới hạn. Input khác như RTMP và HLS đều có giới hạn tổng.

## Hướng sửa
Đặt `DialContext` với `net.Dialer{Timeout: t}` và `TLSHandshakeTimeout: t`, hoặc `context.WithTimeout` tới khi có response header.

## Cách xác minh
Test với listener chấp nhận TCP nhưng không gửi gì, URL `https`: `Open` không trả về sau `timeout_sec`.
