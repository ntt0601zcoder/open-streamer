# Stream code dài vượt giới hạn đường dẫn Unix socket nên transcoder không bao giờ chạy

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, cmd/open-streamer-transcoder, internal/domain
- **Vị trí:** internal/transcoder/supervisor.go:599-604, internal/transcoder/supervisor.go:609-621, cmd/open-streamer-transcoder/main.go:73-76, internal/domain/stream.go:11

## Hiện tượng
Stream có code dài được tạo hợp lệ, nhưng transcoder không khởi động. Log có `listen unix ...: bind: invalid argument`, sau đó `socket ... not bound within 5s`, lặp lại với `restart_count` tăng.

## Điều kiện kích hoạt
Với `os.TempDir()` là `/tmp`, stream code từ 85 ký tự trở lên. `MaxStreamCodeLen` là 128. Stream auto-publish dùng cả đường dẫn đẩy lên làm code nên dễ dài. `TMPDIR` dài hơn sẽ hạ ngưỡng này.

## Phân tích
```go
filepath.Join(os.TempDir(), fmt.Sprintf("ost-%s-%s.sock", safe, hex.EncodeToString(nonce)))
```

Phần cố định là `/tmp/ost-` (9 ký tự) và `-xxxxxxxx.sock` (14 ký tự), tổng 23 ký tự cộng độ dài code. `sun_path` trên Linux giới hạn 108 byte kể cả NUL, nên code quá 84 ký tự sẽ khiến `Listen` thất bại.

## Vì sao đây là lỗi
Validate cho phép tới 128 ký tự và không có ràng buộc nào giữa hai giới hạn. `Service.Start` chỉ probe binary nên không phát hiện. `waitForSocket` cũng không nhận biết subprocess đã thoát nên mỗi vòng mất đủ 5 giây.

## Hướng sửa
Đặt tên socket theo hash ngắn của stream code cộng nonce, không nhúng code vào đường dẫn. Hoặc dùng abstract socket. Có thể kèm kiểm tra subprocess đã thoát trong `waitForSocket`.

## Cách xác minh
Test đơn vị: `len(pickSocketPath(strings.Repeat("a", domain.MaxStreamCodeLen))) < 108`. Thực tế: tạo stream transcode với code 100 ký tự.
