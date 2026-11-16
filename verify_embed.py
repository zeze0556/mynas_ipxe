import sys, gzip

for f in sys.argv[1:]:
    try:
        data = open(f, "rb").read()
    except Exception as e:
        print(f, "-> cannot read:", e)
        continue
    i = 0
    found = False
    while True:
        j = data.find(b"\x1f\x8b\x08", i)
        if j < 0:
            break
        try:
            d = gzip.decompress(data[j : j + 400000])
            if b"chain http" in d or b"next-server" in d or b"boot?mac" in d:
                s = d.decode("latin1")
                m = s.find("chain http")
                print(f, "-> GZIP EMBED FOUND:", s[m : m + 90])
                found = True
                break
        except Exception:
            pass
        i = j + 1
    if not found:
        print(f, "-> NO gzip embed found")
