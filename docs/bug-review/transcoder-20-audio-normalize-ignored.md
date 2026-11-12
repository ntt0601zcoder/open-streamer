# audio.normalize được truyền tới subprocess nhưng không được áp dụng

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/supervisor.go:663, internal/transcoder/native/server.go:372, internal/transcoder/native/pipeline.go:60, internal/transcoder/native/audio_reencode.go:278-310, internal/domain/transcoder.go:140-142, docs/CONFIG.md:291

## Hiện tượng
Bật `normalize: true` nhưng loudness đầu ra không đổi, không đạt -23 LUFS như tài liệu.

## Điều kiện kích hoạt
`transcoder.audio.copy=false` và `normalize=true`.

## Phân tích
Giá trị đi qua `audioConfigFromTC` và `AudioConfigFromProto` vào `AudioConfig.Normalize`, nhưng `audioReencoder` không đọc field này. Đường âm thanh chỉ có decode, resample, FIFO, `applyGain` theo `Volume`, rồi encode. Không có filter `loudnorm` hay xử lý tương đương.

## Vì sao đây là lỗi
Domain và `docs/CONFIG.md` đều công bố tính năng EBU R128, nhưng code không có đường thực thi và không cảnh báo.

## Hướng sửa
Thêm `loudnorm` qua libavfilter trước FIFO, hoặc từ chối cấu hình và gỡ khỏi tài liệu cho tới khi hỗ trợ.

## Cách xác minh
Đo `ffmpeg -af ebur128` trên output với `normalize` bật và tắt. Kết quả hiện được kỳ vọng là giống nhau.
