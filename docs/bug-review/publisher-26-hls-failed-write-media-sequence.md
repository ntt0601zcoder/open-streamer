# Ghi segment HLS lỗi làm lệch MEDIA-SEQUENCE và mất DISCONTINUITY
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS)
- **Vị trí:** internal/publisher/hls.go:688-689, 712-713, 725-732, 791

## Hiện tượng
Khi đĩa đầy tạm thời hoặc lỗi I/O, player có thể phát lặp một segment. Tag DISCONTINUITY dự kiến cho segment lỗi cũng bị mất.

## Điều kiện kích hoạt
`writeFileAtomic` lỗi cho một segment, các lần ghi sau thành công.

## Phân tích
`segN` tăng và `discNext` bị xóa trước khi ghi, còn nhánh lỗi chỉ return:
```go
p.segN++
...
disc := p.discNext
p.discNext = false
if err := writeFileAtomic(path, data); err != nil { ...; return }
```
Manifest tính `mediaSeq := int(p.segN) - len(win)` với giả định `segN` liền mạch. Sau một lỗi, các segment còn lại trong cửa sổ bị tăng sequence thêm 1. Ví dụ `seg_000012` từ sequence 11 thành 12, nên player chờ sequence 12 lại tải đúng segment đã có.

## Vì sao đây là lỗi
Segment không tồn tại nhưng vẫn tiêu một số. Không có guard nào bù lại; DASH có `vSegN--` trên nhánh lỗi cho chính trường hợp này.

## Hướng sửa
Chỉ tăng `segN` và xóa `discNext` sau khi ghi thành công, hoặc lưu số sequence trong từng `hlsSegEntry`.

## Cách xác minh
Unit test: cho `streamDir` không ghi được một lần rồi khôi phục, kiểm tra ánh xạ tên segment và sequence không đổi.
