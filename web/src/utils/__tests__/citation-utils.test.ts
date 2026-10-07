import {
  citationMarkerReg,
  normalizeCitationMarkers,
  parseCitationIndex,
} from '../citation-utils';

describe('normalizeCitationMarkers', () => {
  test('rewrites the full-width brackets OpenAI-family models cite in', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m【ID:1】.')).toBe(
      'The cable run is 50 m[ID:1].',
    );
  });

  test('tolerates the spacing and full-width colon models put inside them', () => {
    expect(normalizeCitationMarkers('a【ID: 1】b【ID：2】c')).toBe(
      'a[ID:1]b[ID:2]c',
    );
  });

  test('rewrites the pre-0.13 ##n$$ form', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m##7$$.')).toBe(
      'The cable run is 50 m[ID:7].',
    );
  });

  test('leaves canonical markers untouched', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m[ID:1].')).toBe(
      'The cable run is 50 m[ID:1].',
    );
  });

  test('leaves ordinary full-width brackets alone', () => {
    // 【】 is regular punctuation in Chinese text; only an ID marker is a citation.
    expect(normalizeCitationMarkers('参见【附录】的说明')).toBe(
      '参见【附录】的说明',
    );
  });

  test('normalised markers are matched by the renderer regex', () => {
    const normalized = normalizeCitationMarkers('a【ID:3】b');
    expect(
      [...normalized.matchAll(citationMarkerReg)].map((m) => m[1]),
    ).toEqual(['3']);
  });
});

describe('parseCitationIndex', () => {
  test('reads a numeric marker as a position', () => {
    expect(parseCitationIndex('[ID:2]')).toBe(2);
  });

  test('keeps a non-numeric marker as a key', () => {
    expect(parseCitationIndex('[ID:a1b2c3d4e5f60718]')).toBe(
      'a1b2c3d4e5f60718',
    );
  });
});
