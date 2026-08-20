import React, { ChangeEvent, useState } from 'react';
import { lastValueFrom } from 'rxjs';
import { css } from '@emotion/css';
import { AppPluginMeta, GrafanaTheme2, PluginConfigPageProps, PluginMeta } from '@grafana/data';
import { getBackendSrv } from '@grafana/runtime';
import { Button, Field, FieldSet, Input, useStyles2 } from '@grafana/ui';
import { testIds } from '../testIds';

type AppPluginSettings = {
  farsightUrl?: string;
};

export interface AppConfigProps extends PluginConfigPageProps<AppPluginMeta<AppPluginSettings>> {}

const AppConfig = ({ plugin }: AppConfigProps) => {
  const s = useStyles2(getStyles);
  const { enabled, pinned, jsonData } = plugin.meta;
  const [farsightUrl, setFarsightUrl] = useState(jsonData?.farsightUrl || '');

  const onChange = (event: ChangeEvent<HTMLInputElement>) => {
    setFarsightUrl(event.target.value.trim());
  };

  const onSubmit = () => {
    updatePluginAndReload(plugin.meta.id, {
      enabled,
      pinned,
      jsonData: { farsightUrl },
    });
  };

  return (
    <form onSubmit={onSubmit}>
      <FieldSet label="Farsight">
        <Field
          label="Farsight server URL"
          description="The Farsight dashboard's Tailscale address, e.g. http://100.x.x.x:8080 — nothing sensitive here, the chat panel talks to this address directly from the browser."
        >
          <Input
            width={60}
            name="farsightUrl"
            id="config-farsight-url"
            data-testid={testIds.appConfig.farsightUrl}
            value={farsightUrl}
            placeholder="http://100.x.x.x:8080"
            onChange={onChange}
          />
        </Field>

        <div className={s.marginTop}>
          <Button type="submit" data-testid={testIds.appConfig.submit} disabled={!farsightUrl}>
            Save
          </Button>
        </div>
      </FieldSet>
    </form>
  );
};

export default AppConfig;

const getStyles = (theme: GrafanaTheme2) => ({
  marginTop: css`
    margin-top: ${theme.spacing(3)};
  `,
});

const updatePluginAndReload = async (pluginId: string, data: Partial<PluginMeta<AppPluginSettings>>) => {
  try {
    await updatePlugin(pluginId, data);
    window.location.reload();
  } catch (e) {
    console.error('Error while updating the plugin', e);
  }
};

const updatePlugin = async (pluginId: string, data: Partial<PluginMeta>) => {
  const response = await getBackendSrv().fetch({
    url: `/api/plugins/${pluginId}/settings`,
    method: 'POST',
    data,
  });

  return lastValueFrom(response);
};
