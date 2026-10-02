import { render, screen } from '@testing-library/react';
import { ReferenceDuplicates } from '../reference-duplicates';

jest.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string, options?: { count?: number }) =>
      `${key}${options?.count !== undefined ? `:${options.count}` : ''}`,
  }),
}));

const duplicate = (chunk_id: string, document_name: string) => ({
  chunk_id,
  document_id: `doc-${chunk_id}`,
  document_name,
  dataset_id: 'kb',
  similarity: 0.5,
});

describe('ReferenceDuplicates', () => {
  it('renders nothing when the chunk has no duplicates', () => {
    const { container } = render(<ReferenceDuplicates duplicates={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('lists each file name once with the number of copies', () => {
    render(
      <ReferenceDuplicates
        duplicates={[
          duplicate('c2', 'permit.pdf'),
          duplicate('c3', 'permit (1).pdf'),
          duplicate('c4', 'permit.pdf'),
        ]}
      />,
    );
    expect(
      screen.getByText('common.alsoFoundIn:3 permit.pdf, permit (1).pdf'),
    ).toBeInTheDocument();
  });
});
