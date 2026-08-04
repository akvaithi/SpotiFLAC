"""Run with: python3 test_matching.py  (stdlib only — no Flask, no Telethon)

The asymmetry these cases encode: accepting the wrong FLAC files someone else's
music under this track's name, and accepting a *correct* file that this check
rejects is a download that simply fails. Both are bad, so the rejections have to
be narrow — every "keep" case below is a real shape seen from @deezload2bot.
"""
import unittest

from matching import matches_expected, norm_core


class FakeAttr:
    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


class FakeMsg:
    """Stands in for a Telethon message carrying a document."""

    def __init__(self, file_name=None, title=None, performer=None, document=True):
        if not document:
            self.document = None
            return
        attrs = []
        if file_name is not None:
            attrs.append(FakeAttr(file_name=file_name))
        if title is not None or performer is not None:
            attrs.append(FakeAttr(title=title, performer=performer))
        self.document = FakeAttr(attributes=attrs)


def msg(**kw):
    return FakeMsg(**kw)


class TestMatchesExpected(unittest.TestCase):
    def test_accepts_the_requested_track(self):
        keep = [
            # The ordinary shape: bot names the file "Artist - Title.flac".
            (msg(file_name="G. V. Prakash Kumar - Vaa Vaathi.flac"), "Vaa Vaathi"),
            # Spotify carries the qualifier, Deezer's filename doesn't.
            (msg(file_name="Vaa Vaathi.flac"), 'Vaa Vaathi (From "Vaathi")'),
            (msg(file_name="Vaa Vaathi.flac"), 'Vaa Vaathi - From "Vaathi"'),
            # ...and the other way around.
            (msg(file_name='Vaa Vaathi (From "Vaathi").flac'), "Vaa Vaathi"),
            # Tags instead of a filename.
            (msg(title="Naatu Naatu", performer="Rahul Sipligunj"), "Naatu Naatu"),
            # Punctuation and case differ.
            (msg(file_name="labh_janjua_london_thumakda.flac"), "London Thumakda"),
        ]
        for m, title in keep:
            with self.subTest(title=title):
                self.assertTrue(matches_expected(m, {"title": title}))

    def test_rejects_a_different_track(self):
        drop = [
            # The actual bug: the previous job's reply, still newest in the chat.
            (msg(file_name="Labh Janjua - London Thumakda.flac"), "Vaa Vaathi"),
            (msg(file_name="Vaathi Coming.flac"), "Vaa Vaathi"),
            (msg(title="Blinding Lights", performer="The Weeknd"), "Naatu Naatu"),
        ]
        for m, title in drop:
            with self.subTest(title=title):
                self.assertFalse(matches_expected(m, {"title": title}))

    def test_fails_open_when_there_is_nothing_to_compare(self):
        # No expectation at all — an older SpotiFLAC calling a newer gateway.
        self.assertTrue(matches_expected(msg(file_name="anything.flac"), {}))
        self.assertTrue(matches_expected(msg(file_name="anything.flac"), {"title": ""}))
        # A filename that normalises to nothing must not fail the download: a
        # Tamil- or Devanagari-script name has no ASCII left to compare.
        self.assertTrue(matches_expected(msg(file_name="வா வாத்தி.flac"), {"title": "Vaa Vaathi"}))
        # A document with no attributes at all.
        self.assertTrue(matches_expected(msg(), {"title": "Vaa Vaathi"}))

    def test_norm_core_drops_version_qualifiers(self):
        self.assertEqual(norm_core('Vaa Vaathi (From "Vaathi")'), "vaavaathi")
        self.assertEqual(norm_core('Vaa Vaathi - From "Vaathi"'), "vaavaathi")
        self.assertEqual(norm_core("Nightcall - Remastered"), "nightcall")
        self.assertNotEqual(norm_core("Vaa Vaathi"), norm_core("Vaathi Coming"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
