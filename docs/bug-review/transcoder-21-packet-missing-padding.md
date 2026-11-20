# Packet đưa vào decoder thiếu AV_INPUT_BUFFER_PADDING_SIZE

- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/transcoder/native, go-astiav
- **Vị trí:** internal/transcoder/native/decoder.go:214-219, internal/transcoder/native/audio_reencode.go:185, go-astiav@v0.41.0/packet.go:170-192

## Hiện tượng
Thường không thấy gì. Về lý thuyết, parser có thể đọc quá cuối vùng cấp phát, gây lỗi bộ nhớ khó tái hiện hoặc SIGSEGV hiếm gặp với packet lớn, hoặc bị công cụ kiểm tra bộ nhớ báo lỗi.

## Điều kiện kích hoạt
Mọi packet video và audio đi qua `Packet.FromData`, nhất là khi vùng cấp phát nằm sát cuối một page.

## Phân tích
Pipeline dùng `FromData`, còn go-astiav cấp phát đúng `len(data)`:

```go
buf := (*C.uint8_t)(C.av_malloc(C.size_t(len(data))))
...
C.av_packet_from_data(p.c, buf, C.int(len(data)))
```

Theo contract của libav, `av_packet_from_data` giả định buffer có sẵn thêm `AV_INPUT_BUFFER_PADDING_SIZE` byte sau `size`. Các bit reader và bộ quét start code tối ưu dựa vào vùng đệm này và có thể đọc quá `size`.

## Vì sao đây là lỗi
Comment ở `decoder.go` còn mô tả sai là "without copying". Không có lớp nào trong repo bổ sung padding. Tác động thực tế chưa được đo, nên đây là vi phạm contract với độ nghiêm trọng thấp, nguyên nhân gốc nằm ở thư viện bên thứ ba.

## Hướng sửa
Cấp phát bản sao qua `av_new_packet` (tự thêm padding) nếu binding hỗ trợ, gửi bản vá lên go-astiav, hoặc truyền vào `FromData` một lát có padding rồi đặt lại size.

## Cách xác minh
Chạy subprocess với build FFmpeg và binary bật AddressSanitizer trong môi trường thử nghiệm riêng, đưa vài phút media vào `Decoder.Decode` và `audioReencoder.Process`.
