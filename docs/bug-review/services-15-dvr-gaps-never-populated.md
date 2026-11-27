# recording_status luôn báo không có gap dù có failover hoặc mất nguồn

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob, internal/api/handler
- **Vị trí:** internal/dvr/blob/catalog.go:83, internal/dvr/blob/service.go:317, internal/dvr/blob/writer.go:103-111, internal/api/handler/blob_timeshift.go:59

## Hiện tượng
`gaps` trong `/<code>/recording_status.json` luôn là `null`, kể cả khi nguồn đã mất nhiều phút hoặc server đã restart. Operator và UI không biết bản ghi có lỗ.

## Điều kiện kích hoạt
Bất kỳ recording nào có gián đoạn: failover (`SessionStart`), nguồn dừng rồi chạy lại, restart.

## Phân tích
Toàn bộ repo chỉ có các tham chiếu sau tới `Gaps`: khai báo struct, phép chép `cat.Gaps = prev.Gaps`, và handler đọc ra. Không nơi nào thêm phần tử. `Ingest` chỉ đặt `pendingDisc` cho fragment kế tiếp, không ghi gap. Trường `HourRecord.Discontinuity` cũng không được gán ở đâu.

## Vì sao đây là lỗi
`docs/ARCHITECTURE.md` mô tả endpoint này trả "DVR status, range, size and gaps", và `Gap` được định nghĩa cho "packet drop, source restart". Giá trị rỗng được hiểu là bản ghi liên tục, tức là thông tin sai chứ không chỉ là thiếu.

## Hướng sửa
Khi cắt fragment có `pendingDisc`, hoặc khi khoảng wall giữa hai fragment vượt `segDur` đáng kể, thêm `Gap{FromMs, ToMs, Reason}` vào catalog dưới `r.mu`, và cắt bớt khi retention prune.

## Cách xác minh
Unit test: `Ingest` hai đoạn cách nhau bởi một packet `SessionStart`, kiểm tra `cat.Gaps` hiện vẫn rỗng.
