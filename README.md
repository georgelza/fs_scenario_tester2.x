# FeatureSpace Fraud Analytics - Scenario post'er

Little app to post various sets of JSON files (from $input_path) to build up a test scenario to test rules/alerts

Create a certs directory and copy client.crt and client.key into it.

1. Create a directory, ie. json_output to receive all output received from the producer.

2. Create a json_source# directory for every set of transaction / scenario's, will be used when json_from_file = 1.

3. Enable/Disable options in dev_app.json or sit_app.json
    Pay attention to the notes next to the various in the *_app.json file.

4. execute fs_producer.exe <argument>
    ie: 
    fs_producer.exe dev
        the argument "dev" is pre pended to _app.json to form dev_app.json
    
    or
    
    fs_producer.exe sit 
        the argument "sit" is pre pended to _app.json to form sit_app.json

    The above argument file then configures the fs_producer environment/options which defines it's behaviour during execution.