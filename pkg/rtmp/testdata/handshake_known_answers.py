# Independent known answers for handshake_test.go.
#
# Usage (from pkg/rtmp):
#   python3 -I testdata/handshake_known_answers.py testdata/ffmpeg_play_handshake.bin \
#       [/usr/lib/x86_64-linux-gnu/libavformat.so.62]
#
# The handshake keys are read from FFmpeg's libavformat binary, not from the
# Go source, so a wrong constant byte in handshake.go cannot leak in here.
# ffmpeg_play_handshake.bin is C0+C1 | S0+S1+S2 | C2 captured with ffmpeg 8
# playing rtmp://127.0.0.1/live/x from our ServerHandshake (random source
# fixedRandom(3072, 2)); ffmpeg verified S2 and went on to send connect.
import hmac, hashlib, sys
lib = open(sys.argv[2] if len(sys.argv) > 2 else '/usr/lib/x86_64-linux-gnu/libavformat.so.62', 'rb').read()
def key(name, n):
    i = -1
    while True:
        i = lib.find(name, i+1)
        if lib[i+len(name)] != 0: return lib[i:i+n]
CK = key(b"Genuine Adobe Flash Player 001", 62)
SK = key(b"Genuine Adobe Flash Media Server 001", 68)
print("clientKey", CK.hex()); print("serverKey", SK.hex())
H = lambda k, d: hmac.new(k, d, hashlib.sha256).digest()
def pos(b, schema):
    base = 8 if schema == 1 else 772
    return base + 4 + sum(b[base:base+4]) % 728
def dig(b, p, k): return H(k, b[:p] + b[p+32:])
def find(b, k):
    for s in (0, 1):
        p = pos(b, s)
        if b[p:p+32] == dig(b, p, k): return s, p
    return None
def fixed(n, seed): return bytes((i*31 + seed) % 256 for i in range(n))

# A. ffmpeg fixture: C0C1 | S0S1S2 | C2
f = open(sys.argv[1], 'rb').read()
c1, s1, s2, c2 = f[1:1537], f[1538:3074], f[3074:4610], f[4610:]
s, p = find(c1, CK[:30]); print("fixture C1 schema", s, "pos", p, "digest", c1[p:p+32].hex())
r = fixed(3072, 2)
exp_s1 = bytearray(b"\0\0\0\0" + bytes.fromhex("0d0e0a0d") + r[:1528])
sp = pos(exp_s1, s); exp_s1[sp:sp+32] = dig(exp_s1, sp, SK[:36])
print("fixture S1 matches independent build:", bytes(exp_s1) == s1, "pos", sp, "digest", exp_s1[sp:sp+32].hex())
exp_s2 = r[1528:1528+1504] + H(H(SK, c1[p:p+32]), r[1528:1528+1504])
print("fixture S2 matches:", exp_s2 == s2, "tail", exp_s2[1504:].hex())
print("ffmpeg C2 signed with H(H(CK62,S1digest)):", H(H(CK, s1[sp:sp+32]), c2[:1504]) == c2[1504:], c2[1504:].hex())

# B. hand-built handshake per schema: client rnd fixed(3032,1), server rnd fixed(3032,2)
for schema in (0, 1):
    cr, sr = fixed(3032, 1), fixed(3032, 2)
    c1 = bytearray(b"\0\0\0\0" + bytes.fromhex("80000702") + cr[:1528])
    cp = pos(c1, schema); c1[cp:cp+32] = dig(c1, cp, CK[:30])
    s1 = bytearray(b"\0\0\0\0" + bytes.fromhex("0d0e0a0d") + sr[:1528])
    sp = pos(s1, schema); s1[sp:sp+32] = dig(s1, sp, SK[:36])
    s2tail = H(H(SK, bytes(c1[cp:cp+32])), sr[1528:3032])
    c2tail = H(H(CK, bytes(s1[sp:sp+32])), cr[1528:3032])
    print(f"schema {schema}: c1pos {cp} c1dig {c1[cp:cp+32].hex()}")
    print(f"schema {schema}: s1pos {sp} s1dig {s1[sp:sp+32].hex()}")
    print(f"schema {schema}: s2tail {s2tail.hex()}")
    print(f"schema {schema}: c2tail {c2tail.hex()}")
