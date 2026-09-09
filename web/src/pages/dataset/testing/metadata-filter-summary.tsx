import {
  IRetrievalMetaFilter,
  IRetrievalMetaFilterCondition,
} from '@/interfaces/database/dataset';
import { useTranslation } from 'react-i18next';

const methodLabelKeys: Record<IRetrievalMetaFilter['method'], string> = {
  auto: 'knowledgeDetails.metadataFilterAuto',
  semi_auto: 'knowledgeDetails.metadataFilterSemiAuto',
  manual: 'knowledgeDetails.metadataFilterManual',
};

const formatValue = (value: IRetrievalMetaFilterCondition['value']) =>
  Array.isArray(value) ? value.join(', ') : String(value ?? '');

/** The search ran unfiltered: auto / semi_auto drop a filter that matched nothing. */
const isIgnored = (filter: IRetrievalMetaFilter) =>
  filter.status === 'no_matches' && filter.method !== 'manual';

/**
 * What the metadata filter resolved to for this run. In `auto` / `semi_auto`
 * the conditions come from the LLM, so this is the only place the reason
 * behind a result set is visible.
 */
export function MetadataFilterSummary({
  filter,
}: {
  filter: IRetrievalMetaFilter;
}) {
  const { t } = useTranslation();
  const joiner =
    filter.logic?.toLowerCase() === 'or'
      ? t('knowledgeDetails.metadataFilterOr')
      : t('knowledgeDetails.metadataFilterAnd');

  return (
    <section className="flex-0 px-5 pb-3">
      <div className="space-y-1.5 rounded-lg border-0.5 border-border-button bg-bg-card px-4 py-2.5 text-xs">
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
          <span className="text-text-secondary">
            {t(methodLabelKeys[filter.method] ?? methodLabelKeys.manual)}
          </span>
          {(filter.status === 'applied' || filter.status === 'no_matches') && (
            <span className="text-text-sub-title-invert">
              {isIgnored(filter)
                ? t('knowledgeDetails.metadataFilterIgnored')
                : t('knowledgeDetails.metadataFilterDocumentCount', {
                    count: filter.matched_document_count,
                  })}
            </span>
          )}
        </div>
        {filter.conditions.length === 0 ? (
          <p className="text-text-sub-title-invert">
            {t('knowledgeDetails.metadataFilterNoConditions')}
          </p>
        ) : (
          <ul className="flex flex-wrap items-center gap-1.5">
            {filter.conditions.map((condition, index) => (
              <li
                key={`${condition.key}-${index}`}
                className="flex items-center gap-1.5"
              >
                {index > 0 && (
                  <span className="uppercase text-text-sub-title-invert">
                    {joiner}
                  </span>
                )}
                <code className="rounded bg-bg-input px-1.5 py-0.5 text-text-primary">
                  {condition.key} {condition.op} {formatValue(condition.value)}
                </code>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}
