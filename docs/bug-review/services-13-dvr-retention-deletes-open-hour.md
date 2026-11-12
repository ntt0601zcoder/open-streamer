# Retention xoá cả hour đang ghi của lane khác

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/dvr/blob
- **Vị trí:** internal/dvr/blob/retention.go:47-59, internal/dvr/blob/retention.go:70-82, internal/dvr/blob/retention.go:98-118, internal/dvr/blob/service.go:222-236

## Hiện tượng
Trên DVR ABR (`dvr.profiles=all`) có `max_size_gb` thấp, hour vừa chuyển giờ của một số profile mất file. Catalog vẫn có hour đó vì lane còn checkpoint, playback trả 404 hoặc `NO_SEGMENTS_IN_RANGE`.

## Điều kiện kích hoạt
1. Từ hai profile trở lên; ngân sách dung lượng nhỏ tới mức hour vừa seal cũng vượt budget.
2. Tại ranh giới giờ, lane A đã seal hour H, lane B chưa (chưa cắt fragment nào của giờ mới, hoặc lane B đứng vì rendition ngừng ra packet).
3. `pruneOnce` chạy trong khoảng đó.

## Phân tích
Hour được chọn từ bất kỳ profile nào đã seal:
```go
if !hr.Sealed { continue }
if !found || hr.Hour < hour { hour, wallToMs, found = hr.Hour, hr.WallToMs, true }
```
Rồi xoá hour đó trên mọi profile, không kiểm tra `Sealed` của từng profile:
```go
for _, ext := range []string{".cmfv", ".cmfa", ".ranges", ".open"} {
    _ = os.Remove(stem + ext)
}
```
Lane B tiếp tục ghi vào file đã unlink, và `checkpointHour` thêm hour đó lại vào catalog qua `upsertHour`. Dung lượng trên đĩa chỉ được trả khi FD đóng.

## Vì sao đây là lỗi
Comment của `runRetention` cam kết "the in-progress ... hour is never deleted". Lane đứng lâu làm khoảng thời gian rủi ro kéo dài hơn nhiều so với vài giây bình thường.

## Hướng sửa
Chỉ xoá hour khi mọi profile có hour đó đều đã seal, hoặc chọn và xoá theo từng profile, luôn loại hour lane đang mở.

## Cách xác minh
Unit test `pruneOnce` với catalog hai profile: p0 có H `Sealed:true`, p1 có H `Sealed:false`, tạo file giả, đặt `MaxSizeGB` nhỏ, kiểm tra file của p1 hiện bị xoá.
