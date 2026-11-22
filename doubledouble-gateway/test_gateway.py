import io
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

from gateway import amazon_link, direct_url, extract_flac


class Downloads(unittest.TestCase):
    def test_only_source_origin_is_allowed(self):
        self.assertEqual(direct_url('./files/test.zip'), 'https://us.doubledouble.top/files/test.zip')
        for value in ['https://pixeldrain.com/u/abc', '//evil.example/a', 'http://us.doubledouble.top/a', 'https://us.doubledouble.top@evil.example/a']:
            with self.assertRaises(ValueError):
                direct_url(value)

    def test_exact_amazon_mapping_requires_the_requested_track(self):
        data = {'props': {'pageProps': {'pageData': {
            'entityData': {'provider':'deezer','type':'song','id':'123'},
            'sections': [{'links':[{'platform':'amazonMusic','url':'https://music.amazon.com/albums/B00WO1YR7K?trackAsin=B00WO1YUQS'}]}]
        }}}}
        page = '<script id="__NEXT_DATA__" type="application/json">' + json.dumps(data) + '</script>'
        self.assertEqual(amazon_link(page, '123'), 'https://music.amazon.com/tracks/B00WO1YUQS?musicTerritory=US')
        with self.assertRaises(ValueError):
            amazon_link(page, '456')
        data['props']['pageProps']['pageData']['sections'][0]['links'][0]['url'] = 'https://music.amazon.com/albums/B00WO1YR7K'
        page = '<script id="__NEXT_DATA__">' + json.dumps(data) + '</script>'
        with self.assertRaises(ValueError):
            amazon_link(page, '123')

    def archive(self, entries):
        buf = io.BytesIO()
        with zipfile.ZipFile(buf, 'w') as archive:
            for name, content in entries.items():
                archive.writestr(name, content)
        buf.seek(0)
        return buf

    def test_single_flac_with_lyrics_is_accepted(self):
        with tempfile.TemporaryDirectory() as folder:
            out = Path(folder) / 'track.flac'
            name = extract_flac(self.archive({'Song.flac': b'fLaCaudio', 'Song.lrc': b'lyrics'}), out)
            self.assertEqual(name, 'Song.flac')
            self.assertEqual(out.read_bytes(), b'fLaCaudio')

    def test_rejects_albums_lossy_audio_and_unsafe_paths(self):
        cases = [
            {'one.flac': b'fLaCa', 'two.flac': b'fLaCb'},
            {'one.mp3': b'mp3'},
            {'one.flac': b'mp3'},
            {'../one.flac': b'fLaCa'},
            {'/one.flac': b'fLaCa'},
        ]
        with tempfile.TemporaryDirectory() as folder:
            for entries in cases:
                with self.assertRaises(ValueError):
                    extract_flac(self.archive(entries), Path(folder) / 'out.flac')

    def test_rejects_corrupt_zip(self):
        raw = bytearray(self.archive({'one.flac': b'fLaCaudio'}).getvalue())
        raw[30 + len('one.flac') + 4] ^= 1
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaises((ValueError, zipfile.BadZipFile)):
                extract_flac(io.BytesIO(raw), Path(folder) / 'out.flac')


if __name__ == '__main__':
    unittest.main()
