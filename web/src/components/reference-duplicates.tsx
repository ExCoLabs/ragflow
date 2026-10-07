import { IDuplicateChunk } from '@/interfaces/database/chat';
import { cn } from '@/lib/utils';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';

type ReferenceDuplicatesProps = {
  duplicates?: IDuplicateChunk[];
  className?: string;
};

/**
 * Where else a retrieved chunk's text was found. Retrieval returns identical
 * chunks once and lists the other copies under `duplicates`; this shows the
 * file names of those copies so a hit from a duplicated file is still traceable.
 */
export function ReferenceDuplicates({
  duplicates,
  className,
}: ReferenceDuplicatesProps) {
  const { t } = useTranslation();
  const names = useMemo(
    () =>
      Array.from(
        new Set(
          (duplicates ?? [])
            .map((x) => x.document_name)
            .filter((x): x is string => Boolean(x)),
        ),
      ),
    [duplicates],
  );

  if (!duplicates?.length) {
    return null;
  }

  return (
    <div
      className={cn('text-xs text-text-secondary break-words', className)}
      title={names.join('\n')}
    >
      {t('common.alsoFoundIn', { count: duplicates.length })} {names.join(', ')}
    </div>
  );
}
