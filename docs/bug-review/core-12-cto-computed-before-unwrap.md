# CTO tính trên PTS/DTS chưa unwrap, B-frame mất CTO ở mốc wrap 26,5 giờ
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** timeline, ingestor/tsnorm
- **Vị trí:** internal/timeline/normaliser.go:392-397,430-432; internal/ingestor/tsnorm/tsnorm.go:655-660

## Hiện tượng
Khoảng mỗi 26,5 giờ của PTS nguồn, vài frame quanh mốc wrap có PTS bằng DTS. Có thể gây giật ngắn hoặc lỗi thứ tự hiển thị với nguồn có B-frame.

## Điều kiện kích hoạt
Nguồn raw-TS có B-frame chạy qua mốc 2^33. Frame có PTS đã wrap nhưng DTS chưa wrap.

## Phân tích
`tsnorm.pesTimestampsMs` không unwrap (khác với `tsdemux`). Normaliser tính:
```go
cto := inPts - inDts   // ≈ -95.443.717 khi chỉ PTS wrap
if cto < 0 { cto = 0 }
```
`unwrapInputLocked` chỉ unwrap DTS và chạy sau khi đã tính `cto`.

## Vì sao đây là lỗi
Việc xử lý wrap đã được làm cẩn thận cho DTS nhưng bỏ sót hiệu `PTS-DTS`. Số frame ảnh hưởng bằng độ sâu reorder.

## Hướng sửa
Khi `cto` âm lớn hơn nửa chu kỳ wrap, cộng thêm `ptsWrapMs` trước khi clamp.

## Cách xác minh
Unit test: seed gần `ptsWrapMs`, Apply packet có DTS=`wrap-10` và PTS=`70`, kiểm tra `PTS-DTS` output bằng 80.
