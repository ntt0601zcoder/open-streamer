# Segment HLS lớn hơn 4 MiB làm gRPC server từ chối và transcoder crash-loop

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** cmd/open-streamer-transcoder, internal/transcoder, internal/ingestor/pull, internal/ingestor
- **Vị trí:** cmd/open-streamer-transcoder/main.go:82, internal/transcoder/supervisor.go:321, internal/transcoder/supervisor.go:339-348, internal/transcoder/native/server.go:82-90, internal/ingestor/pull/hls.go:309, internal/ingestor/worker.go:551-573

## Hiện tượng
Stream transcode có input HLS-pull bitrate cao không ra hình ổn định. `restart_count` tăng theo từng segment, log có `recv event: ... received message larger than max`, stream chuyển `degraded`.

## Điều kiện kích hoạt
Input HLS-pull trong stream có transcoder, với một segment sau `tsnorm` vượt 4 194 304 byte. Ví dụ segment 6 giây ở 6 Mbps (khoảng 4,5 MB) hoặc 10 giây ở 4 Mbps (khoảng 5 MB).

## Phân tích
`HLSReader` đưa nguyên cả segment thành một chunk (`hlsResult{data: data}`, giới hạn đến 64 MiB). `writeRawTSChunk` chạy `tsNormaliser.Process(cp)` và ghi toàn bộ kết quả trong một `buffer.Packet{TS: payload}`. Supervisor gửi nguyên gói:

```go
data := pkt.TS
...
stream.Send(&pb.Request{Body: &pb.Request_Packet{Packet: &pb.InputPacket{Data: data, ...}}})
```

Subprocess tạo server không có option:

```go
srv := grpc.NewServer()
```

Giới hạn nhận mặc định của grpc-go server là 4 MiB. Client gửi được (giới hạn gửi mặc định rất lớn), nên server trả `ResourceExhausted`, `Server.Run` thoát ở `stream.Recv()`, `outputDrain` nhận lỗi và supervisor respawn.

## Vì sao đây là lỗi
Toàn repo không có `MaxRecvMsgSize` hay `MaxCallRecvMsgSize`. Không có bước chia nhỏ TS trước `stream.Send`. Respawn không giúp được vì segment kế tiếp cũng vượt giới hạn. Các input UDP/SRT dùng chunk nhỏ nên không lộ lỗi này.

## Hướng sửa
Chia `pkt.TS` thành nhiều `InputPacket` có kích thước giới hạn ở `forwardOnePacket`, chỉ giữ `SessionStart` ở mảnh đầu. Hoặc đặt `grpc.MaxRecvMsgSize` phía server và `MaxCallRecvMsgSize` phía client theo trần đã chọn (ít nhất bằng `hlsMaxSegmentBytes` cộng overhead của remux).

## Cách xác minh
Test tích hợp với `grpc.NewServer()` như production: gửi `InputPacket` 5 MiB qua Unix socket và kiểm tra client nhận `ResourceExhausted`. Thực tế: pull một HLS có segment trên 4 MiB vào stream transcode và theo dõi `restart_count`.
