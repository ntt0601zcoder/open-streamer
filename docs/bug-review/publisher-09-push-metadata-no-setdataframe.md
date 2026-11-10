# RTMP push gửi onMetaData không có `@setDataFrame`
- **Mức độ:** Thấp
- **Độ tin cậy:** Thấp
- **Module liên quan:** publisher (RTMP push), lal
- **Vị trí:** internal/publisher/push_codec.go:191-220; internal/ingestor/push/rtmp_writer.go:193

## Hiện tượng
Đích RTMP chặt có thể bỏ qua metadata, không biết codec hint trước khi nhận media, hoặc từ chối publish.

## Điều kiện kích hoạt
Push tới đích chỉ nhận metadata dạng `@setDataFrame onMetaData`.

## Phân tích
Comment nói phát `@setDataFrame onMetaData`, nhưng code gọi `rtmp.BuildMetadata`, và hàm này trong lal v0.37.4 chỉ ghi chuỗi `onMetaData`. Code không gọi `MetadataEnsureWithSdf`, trong khi đường RTMP play có gọi và giải thích lý do ngay tại `rtmp_writer.go:175-182`.

## Vì sao đây là lỗi
Hành vi trái với comment của chính hàm và không nhất quán với đường play. Chưa xác nhận được đích cụ thể nào từ chối, nên độ tin cậy thấp.

## Hướng sửa
Gọi `rtmp.MetadataEnsureWithSdf(bMeta)` trước khi tạo message.

## Cách xác minh
Bắt tag đầu tiên ở server nhận và kiểm tra chuỗi AMF0 đầu là `@setDataFrame`. Thử trên các đích thật mà hệ thống cần hỗ trợ.
