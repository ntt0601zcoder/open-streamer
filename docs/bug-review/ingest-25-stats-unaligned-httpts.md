# Bộ quét stats giả định chunk thẳng hàng 188 byte, panel tracks của HTTP-TS sai hoặc trống
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, manager
- **Vị trí:** internal/ingestor/pull/stats_pmt_scanner.go:158-168; internal/ingestor/pull/httpts.go:115-118; internal/ingestor/worker.go:549

## Hiện tượng
Input HTTP-TS có thể hiện "No tracks reported", bitrate thấp hơn thực tế hoặc dao động, dù dữ liệu chạy bình thường.

## Điều kiện kích hoạt
Input HTTP-TS, nơi `Body.Read` trả số byte bất kỳ, không phải bội của 188.

## Phân tích
`scan` chia từ offset 0 của chunk:
```go
for i := 0; i+tsPacketSize <= len(chunk); i += tsPacketSize {
    s.processPacket(chunk[i : i+tsPacketSize])
}
```
`processPacket` bỏ nếu `pkt[0] != 0x47`. Chunk bắt đầu giữa gói làm tất cả gói trong chunk bị bỏ, kể cả PAT/PMT. Phần dư cuối chunk cũng bị mất. Không có carry như `MPTSProgramFilter`.

## Vì sao đây là lỗi
Stats được thêm chính để panel không trống với "UDP / HTTP-MPEG-TS / SRT / File". Các chunk của UDP/HLS/SRT/file thẳng hàng nên lỗi chỉ lộ ở HTTP-TS. Đường dữ liệu không bị ảnh hưởng.

## Hướng sửa
Thêm carry và tìm sync trong `statsPMTScanner.scan`, dùng chung logic với `MPTSProgramFilter.filter`.

## Cách xác minh
Unit test `scan` với TS hợp lệ cắt thành các chunk 1000 byte: đếm số lần `onAV` so với chunk thẳng hàng.
