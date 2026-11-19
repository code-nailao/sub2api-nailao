"""Content-addressed tar contexts avoid timestamp/size-only local copy caches."""
from pathlib import Path
import tarfile


def create_context(directory, destination):
    directory=Path(directory).resolve();destination=Path(destination).absolute()
    if destination.is_relative_to(directory):raise ValueError('context archive must be outside input directory')
    with destination.open('xb') as output:
        with tarfile.open(fileobj=output,mode='w',format=tarfile.PAX_FORMAT) as archive:
            for file in sorted(directory.rglob('*')):
                if file.is_symlink() or (not file.is_file() and not file.is_dir()):raise ValueError('context must contain regular files/directories only')
                entry=archive.gettarinfo(str(file),arcname=file.relative_to(directory).as_posix())
                entry.uid=entry.gid=entry.mtime=0;entry.uname=entry.gname=''
                entry.mode=0o755 if file.is_dir() or file.stat().st_mode&0o111 else 0o644
                if file.is_file():
                    with file.open('rb') as stream:archive.addfile(entry,stream)
                else:archive.addfile(entry)
