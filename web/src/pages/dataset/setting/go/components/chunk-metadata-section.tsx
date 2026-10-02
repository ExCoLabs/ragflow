import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Label } from '@/components/ui/label';
import { MultiSelect } from '@/components/ui/multi-select';
import { Switch } from '@/components/ui/switch';
import {
  useFetchKnowledgeMetadata,
  useKnowledgeBaseId,
} from '@/hooks/use-knowledge-request';
import {
  fetchChunkMetadata,
  runChunkMetadataBackfill,
  updateChunkMetadata,
} from '@/services/knowledge-service';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { isEqual } from 'lodash';
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';

// GET/PUT /datasets/:id/chunk_metadata. "ready" is owned by the backfill: the
// server resets it when the fields change, so this section only edits
// enabled/fields and starts the backfill.
interface ChunkMetadataStatus {
  supported: boolean;
  active: boolean;
  config: { enabled: boolean; fields: string[]; ready: boolean } | null;
  backfill?: {
    running: boolean;
    error?: string;
    groups: number;
    groups_done: number;
  };
}

const QUERY_KEY = 'fetchChunkMetadata';

async function unwrap<T>(promise: Promise<any>): Promise<T> {
  const { data } = await promise;
  if (data?.code !== 0) {
    throw new Error(data?.message || 'request failed');
  }
  return data.data as T;
}

export function ChunkMetadataSection() {
  const { t } = useTranslation();
  const datasetId = useKnowledgeBaseId();
  const queryClient = useQueryClient();

  const { data: status } = useQuery<ChunkMetadataStatus>({
    queryKey: [QUERY_KEY, datasetId],
    enabled: !!datasetId,
    queryFn: () => unwrap(fetchChunkMetadata(datasetId)),
    refetchInterval: (query) =>
      query.state.data?.backfill?.running ? 3000 : false,
  });

  const saved = useMemo(
    () => ({
      enabled: status?.config?.enabled ?? false,
      fields: status?.config?.fields ?? [],
    }),
    [status?.config],
  );
  const [enabled, setEnabled] = useState(false);
  const [fields, setFields] = useState<string[]>([]);
  useEffect(() => {
    setEnabled(saved.enabled);
    setFields(saved.fields);
  }, [saved]);
  const dirty = enabled !== saved.enabled || !isEqual(fields, saved.fields);

  const kbIds = useMemo(() => (datasetId ? [datasetId] : []), [datasetId]);
  const { data: metadata } = useFetchKnowledgeMetadata(kbIds);
  const options = useMemo(() => {
    // Keep selected keys visible even if no document currently carries them.
    const keys = new Set([...Object.keys(metadata ?? {}), ...fields]);
    return [...keys].sort().map((key) => ({ label: key, value: key }));
  }, [metadata, fields]);

  const refresh = (next: ChunkMetadataStatus) =>
    queryClient.setQueryData([QUERY_KEY, datasetId], next);

  const save = useMutation({
    mutationFn: () =>
      unwrap<ChunkMetadataStatus>(
        updateChunkMetadata(datasetId, { enabled, fields }),
      ),
    onSuccess: (next) => {
      refresh(next);
      toast.success(t('knowledgeConfiguration.chunkMetadataSaved'));
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const backfill = useMutation({
    mutationFn: () => unwrap(runChunkMetadataBackfill(datasetId)),
    onSuccess: () => {
      toast.success(t('knowledgeConfiguration.chunkMetadataBackfillQueued'));
      queryClient.invalidateQueries({ queryKey: [QUERY_KEY, datasetId] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  if (status && !status.supported) {
    return (
      <p className="text-sm text-text-secondary">
        {t('knowledgeConfiguration.chunkMetadataUnsupported')}
      </p>
    );
  }

  const running = !!status?.backfill?.running;
  const ready = !!status?.config?.ready;

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between gap-4">
        <Label
          htmlFor="chunk-metadata-enabled"
          title={t('knowledgeConfiguration.chunkMetadataTip')}
        >
          {t('knowledgeConfiguration.chunkMetadataEnabled')}
        </Label>
        <Switch
          id="chunk-metadata-enabled"
          checked={enabled}
          onCheckedChange={setEnabled}
        />
      </div>
      <p className="text-xs text-text-secondary">
        {t('knowledgeConfiguration.chunkMetadataTip')}
      </p>
      {enabled && (
        <div className="space-y-2">
          <Label title={t('knowledgeConfiguration.chunkMetadataFieldsTip')}>
            {t('knowledgeConfiguration.chunkMetadataFields')}
          </Label>
          <MultiSelect
            options={options}
            value={fields}
            defaultValue={saved.fields}
            onValueChange={setFields}
            placeholder={t('common.pleaseSelect')}
            maxCount={6}
            modalPopover
          />
        </div>
      )}
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex items-center gap-2 text-sm">
          <span>{t('knowledgeConfiguration.chunkMetadataStatus')}</span>
          <Badge variant={ready ? 'success' : 'secondary'}>
            {ready
              ? t('knowledgeConfiguration.chunkMetadataReady')
              : t('knowledgeConfiguration.chunkMetadataNotReady')}
          </Badge>
          {running && (
            <span className="text-xs text-text-secondary">
              {t('knowledgeConfiguration.chunkMetadataBackfillRunning', {
                done: status?.backfill?.groups_done ?? 0,
                total: status?.backfill?.groups ?? 0,
              })}
            </span>
          )}
        </div>
        <div className="flex gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!datasetId || !dirty || save.isPending}
            onClick={() => save.mutate()}
          >
            {t('knowledgeConfiguration.chunkMetadataSave')}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={
              !datasetId ||
              dirty ||
              !saved.enabled ||
              saved.fields.length === 0 ||
              running ||
              backfill.isPending
            }
            onClick={() => backfill.mutate()}
          >
            {t('knowledgeConfiguration.chunkMetadataBackfill')}
          </Button>
        </div>
      </div>
      {dirty && (
        <p className="text-xs text-text-secondary">
          {t('knowledgeConfiguration.chunkMetadataSaveFirst')}
        </p>
      )}
      {!running && status?.backfill?.error && (
        <p className="text-xs text-state-error">
          {t('knowledgeConfiguration.chunkMetadataBackfillFailed', {
            error: status.backfill.error,
          })}
        </p>
      )}
    </div>
  );
}
