import hashlib
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from build_context import create_context


class ContextTests(unittest.TestCase):
    def test_same_size_same_time_changed_bytes_change_context(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);context=root/'input';context.mkdir();file=context/'index.html'
            file.write_bytes(b'old-js');os.utime(file,(0,0));create_context(context,root/'old.tar')
            file.write_bytes(b'new-js');os.utime(file,(0,0));create_context(context,root/'new.tar')
            self.assertNotEqual(hashlib.sha256((root/'old.tar').read_bytes()).digest(),hashlib.sha256((root/'new.tar').read_bytes()).digest())
            with tarfile.open(root/'new.tar') as archive:self.assertEqual(archive.extractfile('index.html').read(),b'new-js')
    def test_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);context=root/'input';context.mkdir();(context/'escape').symlink_to('/etc/passwd')
            with self.assertRaises(ValueError):create_context(context,root/'archive.tar')


if __name__=='__main__':unittest.main()
