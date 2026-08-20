import React from 'react';
import { AppRootProps } from '@grafana/data';
import Chat from '../../pages/Chat';

function App(props: AppRootProps) {
  return <Chat meta={props.meta} />;
}

export default App;
