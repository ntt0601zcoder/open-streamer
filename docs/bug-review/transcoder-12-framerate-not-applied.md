# framerate của profile và global.fps không đổi được tốc độ khung hình output

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/supervisor.go:552, internal/transcoder/supervisor.go:580-583, internal/transcoder/native/server.go:284, internal/transcoder/native/server.go:432-437, internal/transcoder/native/encoder.go:120, internal/transcoder/native/stream_pipeline.go:1346-1387, internal/domain/transcoder.go:84-85, docs/CONFIG.md:279, docs/CONFIG.md:300

## Hiện tượng
Đặt `framerate: 30` cho nguồn 50 fps nhưng output vẫn 50 fps. Bitrate và khoảng cách GOP thực tế cũng lệch với cấu hình, vì encoder tính theo fps đã khai báo. Khi để `0 = match source`, encoder luôn được báo 25 fps.

## Điều kiện kích hoạt
`framerate` hoặc `global.fps` khác fps nguồn. Hoặc `framerate=0` với nguồn khác 25 fps.

## Phân tích
`encodeOne` chuyển mỗi frame decode thành đúng một lần encode cho mọi rendition. Không có bước drop, duplicate hay filter `fps`. `Framerate` chỉ đến `SetFramerate` của encoder:

```go
cc.SetFramerate(astiav.NewRational(cfg.Framerate, 1))
```

Giá trị 0 bị đổi thành 25 ở `framerateOrDefault`, phần lẻ bị cắt (`29.97` thành 29). `Global.FPS` chỉ được dùng trong `resolveGopFrames`, không gửi qua proto. GOP tính theo frame nên với nguồn 50 fps và fps khai báo 25, GOP thực tế chỉ bằng nửa số giây mong muốn.

## Vì sao đây là lỗi
`docs/CONFIG.md` và comment domain mô tả đây là tốc độ khung hình output. Comment test còn nhắc "reserved for P5 fps-conversion", nghĩa là tính năng chưa được làm nhưng cấu hình vẫn được nhận mà không cảnh báo.

## Hướng sửa
Thêm bộ chuyển fps theo PTS cho từng rendition (drop/duplicate hoặc filter `fps`), truyền `Global.FPS` qua proto và dùng rational cho fps lẻ. Với `0`, lấy fps quan sát được từ nguồn thay vì 25. Nếu chưa làm, từ chối hoặc cảnh báo khi cấu hình.

## Cách xác minh
Transcode nguồn 50 fps với `framerate: 25`, ffprobe `avg_frame_rate` của rendition và đo khoảng cách IDR.
