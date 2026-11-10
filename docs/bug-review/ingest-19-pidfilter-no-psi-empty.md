# Allowlist pids không có PMT cho ra stream rỗng, trái với mục đích xử lý PSI lỗi
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor/tsnorm, domain
- **Vị trí:** internal/ingestor/pull/pid_filter.go:6-9, 13-19; internal/domain/input.go:65-69; internal/ingestor/tsnorm/tsnorm.go:429-433; internal/ingestor/worker.go:560-571

## Hiện tượng
Operator dùng `pids` cho encoder có PAT/PMT lỗi, hoặc liệt kê các PID ES mà không kèm PID PMT: không có packet nào ra, input timeout rồi Degraded. Không có log cảnh báo.

## Điều kiện kích hoạt
Input UDP đặt `pids: [256, 257]`, không có PAT/PMT hợp lệ trong danh sách hoặc từ nguồn. Đây đúng là tình huống cờ này được mô tả để xử lý.

## Phân tích
`PIDFilter` chuyển đúng các PID được chọn. Trong tsnorm, PES chỉ được xử lý nếu PID đã thấy trong PMT:
```go
st, ok := n.pidStream[data.PID]
if !ok {
    continue
}
```
Không có PMT, không có frame nào được chuyển; `Process` trả rỗng và `writeRawTSChunk` trả nil mà không ghi hub. Luồng này không đi nhánh fallback passthrough vì không có lỗi demux.

## Vì sao đây là lỗi
`domain/input.go` ghi cờ này "Used when the source PSI is unreliable (legacy encoders with malformed PAT/PMT)" và `pid_filter.go` nêu use case "Source PSI is malformed or missing". Trước khi tsnorm được đưa vào, byte vẫn được chuyển tiếp. Yêu cầu "operator phải kèm PID PSI" không thể thực hiện nếu nguồn không có PSI dùng được.

## Hướng sửa
Khi `pids` được đặt mà không có PMT, đi đường passthrough có cảnh báo, hoặc thêm cấu hình ánh xạ PID sang codec để tsnorm dùng thay PMT. Cảnh báo khi sau một khoảng thời gian có TS đầu vào nhưng không có PES nào được nhận.

## Cách xác minh
Unit test: chuỗi `PIDFilter` với tsnorm, loại PID PMT khỏi danh sách, kiểm tra output rỗng. Thủ công: input UDP đặt `pids` chỉ gồm PID video và audio.
