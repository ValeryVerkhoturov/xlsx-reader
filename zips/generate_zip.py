#!/usr/bin/env python3
import os
import random
import string
import zipfile

TARGET_GB = 3
TARGET_BYTES = TARGET_GB * 1024 ** 3
ENTRIES = 3
CHUNK_SIZE = 4 * 1024 * 1024   # 4 MB — one pre-built buffer reused for all writes


def make_chunk() -> bytes:
    alphabet = (string.ascii_letters + string.digits + "   \n").encode()
    return bytes(random.choices(alphabet, k=CHUNK_SIZE))


def write_entry(dst, total: int, chunk: bytes, label: str) -> None:
    written = 0
    report_every = max(total // 20, CHUNK_SIZE)  # ~5 % increments
    next_report = report_every
    while written < total:
        n = min(len(chunk), total - written)
        dst.write(chunk[:n])
        written += n
        if written >= next_report or written == total:
            print(f"    {label}: {written / 1024**3:.2f} / {total / 1024**3:.2f} GB", end="\r")
            next_report += report_every
    print()


def entry_sizes(total: int, count: int) -> list[int]:
    base, rem = divmod(total, count)
    return [base + (1 if i < rem else 0) for i in range(count)]


def make_classic_zip(path: str) -> None:
    sizes = entry_sizes(TARGET_BYTES, ENTRIES)
    chunk = make_chunk()
    print(f"Writing {path}  ({TARGET_GB} GB, {ENTRIES} entries, ZIP_STORED, 32-bit) ...")

    saved = zipfile.ZIP64_LIMIT
    zipfile.ZIP64_LIMIT = (1 << 32) - 2   # true uint32 max minus the sentinel value
    try:
        with zipfile.ZipFile(path, "w", zipfile.ZIP_STORED, allowZip64=False) as zf:
            for i, size in enumerate(sizes):
                info = zipfile.ZipInfo(f"random_{i:02d}.txt")
                with zf.open(info, "w") as dst:
                    write_entry(dst, size, chunk, f"entry {i} ({size // 1024**2} MB)")
    finally:
        zipfile.ZIP64_LIMIT = saved

    print(f"  done: {os.path.getsize(path) / 1024**3:.2f} GB on disk\n")


def make_zip64(path: str) -> None:
    sizes = entry_sizes(TARGET_BYTES, ENTRIES)
    chunk = make_chunk()
    print(f"Writing {path}  ({TARGET_GB} GB, {ENTRIES} entries, ZIP_STORED, ZIP64) ...")

    saved = zipfile.ZIP64_LIMIT
    zipfile.ZIP64_LIMIT = 0   # force zip64 for every entry regardless of size
    try:
        with zipfile.ZipFile(path, "w", zipfile.ZIP_STORED, allowZip64=True) as zf:
            for i, size in enumerate(sizes):
                info = zipfile.ZipInfo(f"random_{i:02d}.txt")
                with zf.open(info, "w", force_zip64=True) as dst:
                    write_entry(dst, size, chunk, f"entry {i} ({size // 1024**2} MB)")
    finally:
        zipfile.ZIP64_LIMIT = saved

    print(f"  done: {os.path.getsize(path) / 1024**3:.2f} GB on disk\n")


def describe(path: str) -> None:
    """Print a structural summary without loading the whole file into memory."""
    size = os.path.getsize(path)

    tail_len = min(size, 128 * 1024)
    with open(path, "rb") as f:
        f.seek(-tail_len, 2)
        tail = f.read()

    has_z64_eocd = b"PK\x06\x06" in tail   # ZIP64 end of central directory
    has_z64_loc  = b"PK\x06\x07" in tail   # ZIP64 EOCD locator
    has_eocd     = b"PK\x05\x06" in tail   # classic end of central directory

    with zipfile.ZipFile(path) as zf:
        infos = zf.infolist()

    print(f"{path}")
    print(f"  size on disk        : {size:,} bytes  ({size / 1024**3:.2f} GB)")
    print(f"  entries             : {len(infos)}")
    for info in infos:
        print(f"    {info.filename}  {info.file_size / 1024**3:.2f} GB uncompressed")
    print(f"  classic EOCD (PK56) : {has_eocd}")
    print(f"  ZIP64 EOCD  (PK66)  : {has_z64_eocd}")
    print(f"  ZIP64 locator(PK67) : {has_z64_loc}")
    print(f"  => {'ZIP64 / 64-bit' if has_z64_eocd else 'classic / 32-bit'} format")
    print()


if __name__ == "__main__":
    make_classic_zip("classic_32bit.zip")
    make_zip64("zip64_64bit.zip")

    describe("classic_32bit.zip")
    describe("zip64_64bit.zip")
